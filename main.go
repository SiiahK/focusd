package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// staticFS embute os assets servidos localmente (htmx vendorizado) no próprio
// binário — nada de CDN externo. Local-first, privado, offline, um único artefato.
//
//go:embed static
var staticFS embed.FS

// viewsFS embute os templates HTML no binário. Com isso o daemon roda de QUALQUER
// diretório (não depende mais de um views/ em disco no CWD): um único artefato,
// pré-requisito para o modo daemon "atrito zero".
//
//go:embed views/*.html
var viewsFS embed.FS

// requestTimeout é o orçamento SERVER-SIDE de cada request (Fase 3). O cliente
// de terminal já desiste em 250ms; sem deadline no servidor, o handler órfão
// continuaria rodando e, com o banco travado por um processo externo, os órfãos
// se empilhariam (busy_timeout segura cada um por até 5s). Com o deadline no
// context, o modernc interrompe a query no ponto em que ela estiver.
const requestTimeout = 2 * time.Second

// maxHeartbeatBatch limita o lote do POST /heartbeat. O plugin coalesce por
// arquivo e raramente passa de dezenas; um lote maior que isto é cliente
// bugado ou abuso, e recusamos antes de abrir transação.
const maxHeartbeatBatch = 1000

// heartbeatPayload é o envelope JSON do POST /heartbeat (contrato com o
// focusd.nvim; ver Heartbeat em db.go para o formato de cada item).
type heartbeatPayload struct {
	Source     string      `json:"source"`
	Heartbeats []Heartbeat `json:"heartbeats"`
}

// env devolve o valor da variável de ambiente {key} ou {fallback} se vazia.
// Mantém os defaults históricos (focus.db / :8080) — zero breaking change.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Template implementa echo.Renderer usando html/template.
// Como cada bloco é nomeado via {{define}}, podemos renderizar tanto a
// página inteira quanto fragmentos isolados (HTMX) com o mesmo conjunto.
type Template struct {
	templates *template.Template
}

func (t *Template) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

// focusStatusData é o payload compartilhado da página e do fragmento de foco.
// Serve tanto para renderizar o index inteiro quanto o painel #focus-status.
type focusStatusData struct {
	Habits    []Habit
	Active    *ActiveFocus
	HabitName string // nome do hábito em foco (só quando Active != nil)
	Minutes   int    // minutos decorridos (só quando Active != nil)
	OOB       bool   // se true, o painel é renderizado como swap out-of-band (DELETE)
}

// statusText formata o estado de foco para a barra de status (texto puro).
func statusText(active *ActiveFocus) string {
	if active == nil {
		return "Nenhum foco ativo"
	}
	return fmt.Sprintf("🎯 Foco: %dm", active.MinutesElapsed())
}

// isHTMX indica se a requisição veio do HTMX (navegador) e não de um cliente
// de terminal (curl do Neovim/tmux), que espera texto puro.
func isHTMX(c echo.Context) bool {
	return c.Request().Header.Get("HX-Request") == "true"
}

// focusStatusPayload monta o estado atual de foco (hábitos + sessão ativa).
func focusStatusPayload(ctx context.Context, store *Store) (focusStatusData, error) {
	habits, err := store.GetHabits(ctx)
	if err != nil {
		return focusStatusData{}, err
	}
	active, err := store.GetActiveFocus(ctx)
	if err != nil {
		return focusStatusData{}, err
	}
	d := focusStatusData{Habits: habits, Active: active}
	if active != nil {
		d.Minutes = active.MinutesElapsed()
		for _, h := range habits {
			if h.ID == active.HabitID {
				d.HabitName = h.Name
				break
			}
		}
		if d.HabitName == "" {
			d.HabitName = fmt.Sprintf("hábito %d", active.HabitID)
		}
	}
	return d, nil
}

// renderFocusStatus devolve o fragmento HTML do painel de foco (HTMX).
func renderFocusStatus(c echo.Context, store *Store) error {
	d, err := focusStatusPayload(c.Request().Context(), store)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "focus-status", d)
}

func main() {
	// Com subcomando o binário é CLI (cli.go); sem argumentos, é o daemon.
	if len(os.Args) > 1 {
		runCLI(os.Args[1:])
		return
	}

	// Instância única (flock): se outro focusd já roda, saímos limpos e
	// SILENCIOSOS (exit 0). É disto que o autospawn dos clientes depende —
	// dois disparos simultâneos: um vence o lock, o outro sai sem ruído.
	lock, err := acquireSingleton(lockPath())
	if err != nil {
		os.Exit(0)
	}
	defer lock.Close()

	// Log rotativo ANTES de qualquer coisa que possa logar: o daemon é
	// invisível, NUNCA cospe no terminal do usuário. Tudo vai para o arquivo.
	logw := newRotatingLogger()
	silenceStdlog(logw)

	// Config por ambiente:
	//   FOCUSD_DB   — caminho do banco SQLite (default: focus.db no CWD)
	//   FOCUSD_SOCK — caminho do Unix socket (default: $XDG_RUNTIME_DIR/focusd/)
	//   FOCUSD_ADDR — TCP OPCIONAL para a UI de navegador. VAZIO por padrão:
	//                 o canal de IPC agora é o socket Unix; TCP é opt-in.
	dbPath := env("FOCUSD_DB", "focus.db")
	tcpAddr := os.Getenv("FOCUSD_ADDR")

	store, err := NewStore(dbPath)
	if err != nil {
		log.Fatalf("falha ao inicializar o banco: %v", err)
	}
	defer store.Close()

	// Templates lidos do FS EMBUTIDO — o binário é autossuficiente.
	t := &Template{
		templates: template.Must(template.ParseFS(viewsFS, "views/*.html")),
	}

	e := echo.New()
	e.HideBanner = true // nada de banner ASCII no stdout: daemon silencioso
	e.HidePort = true
	e.Logger.SetOutput(logw) // logger interno do echo → arquivo rotativo
	e.Renderer = t
	// Logger com Skipper: os endpoints de polling (tmux a cada 1s, lualine a
	// cada 5s, HTMX a cada 15s) gerariam ~110k linhas/dia de ruído num daemon
	// que roda para sempre. Loga só o que é mutação/página, e no arquivo.
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Output: logw,
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/status" || c.Path() == "/focus/status" || c.Path() == "/heartbeat"
		},
	}))
	e.Use(middleware.Recover())

	// Timeout server-side (Fase 3): todo request nasce com deadline, e esse
	// context viaja até o Store (QueryContext/ExecContext). Não usamos o
	// middleware.Timeout do echo (que troca de goroutine e responde 503 por
	// cima do handler — fonte conhecida de data race): aqui só armamos o
	// deadline; quem aborta é a própria query, no ponto em que estiver.
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ctx, cancel := context.WithTimeout(c.Request().Context(), requestTimeout)
			defer cancel()
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	})

	// GET / — renderiza a página completa com a lista atual de hábitos e o
	// estado de foco corrente.
	e.GET("/", func(c echo.Context) error {
		d, err := focusStatusPayload(c.Request().Context(), store)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.Render(http.StatusOK, "index", d)
	})

	// GET /static/* — assets embutidos no binário via embed.FS (htmx vendorizado).
	// O caminho da URL casa 1:1 com o diretório "static" no FS embutido, então o
	// http.FileServer resolve /static/htmx.min.js direto, sem CDN, sem rede externa.
	e.GET("/static/*", echo.WrapHandler(http.FileServer(http.FS(staticFS))))

	// GET /focus/status — fragmento HTML do painel de foco (usado no polling HTMX).
	e.GET("/focus/status", func(c echo.Context) error {
		return renderFocusStatus(c, store)
	})

	// GET /habits — retorna apenas o fragmento da lista de hábitos.
	e.GET("/habits", func(c echo.Context) error {
		habits, err := store.GetHabits(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.Render(http.StatusOK, "habit-list", habits)
	})

	// POST /habits — cria um hábito e devolve fragmentos HTML.
	// O item novo entra no topo da lista e o <select> de foco é ressincronizado
	// via out-of-band swap. O reset do formulário é client-side (hx-on) para não
	// poluir a resposta com HTML de form.
	e.POST("/habits", func(c echo.Context) error {
		name := c.FormValue("name")
		if name == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "name é obrigatório")
		}
		frequency := c.FormValue("frequency")
		if frequency == "" {
			frequency = "daily"
		}

		id, err := store.CreateHabit(c.Request().Context(), name, c.FormValue("description"), frequency)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		// Estado completo pós-inserção (mesma receita do DELETE): além do item
		// novo, sincroniza via OOB tudo que depende da lista de hábitos.
		d, err := focusStatusPayload(c.Request().Context(), store)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		d.OOB = true

		newHabit := Habit{ID: id, Name: name, Description: c.FormValue("description"), Frequency: frequency}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
		c.Response().WriteHeader(http.StatusOK)
		// 1) o item novo é inserido no topo da lista (hx-swap="afterbegin")
		if err := t.templates.ExecuteTemplate(c.Response(), "habit-item", newHabit); err != nil {
			return err
		}
		// 2) o select do log manual é ressincronizado via OOB
		if err := t.templates.ExecuteTemplate(c.Response(), "habit-select", d.Habits); err != nil {
			return err
		}
		// 3) primeiro hábito: remove o placeholder "nenhum hábito ainda", que o
		//    afterbegin deixaria visível ao lado do item recém-criado
		if len(d.Habits) == 1 {
			if _, err := io.WriteString(c.Response(), `<li id="habit-empty" hx-swap-oob="delete"></li>`); err != nil {
				return err
			}
		}
		// 4) painel de foco via OOB: o painel ocioso não faz polling de propósito
		//    (para não resetar o <select> em uso), então sem isto o botão ▶ start
		//    só apareceria após um F5 ao criar o primeiro hábito
		return t.templates.ExecuteTemplate(c.Response(), "focus-status", d)
	})

	// DELETE /habits/:id — remove um hábito. A resposta não tem "corpo principal":
	// o alvo primário (#habit-{id}, outerHTML) recebe conteúdo vazio e o <li> some.
	// Junto vão DOIS swaps out-of-band, mantendo tudo em sincronia sem reload
	// (filosofia Big HTML — o servidor devolve HTML pronto, zero JS):
	//   1) #habit-select — opções do <select> de log manual;
	//   2) #focus-status — painel de foco. Se o hábito apagado estava em foco, o
	//      ON DELETE CASCADE (foreign_keys=on) já encerrou o active_focus, então
	//      o painel volta a "ocioso" na hora; caso contrário, só perde a opção
	//      do <select> ocioso — fechando o buraco do FK 500 em /focus/start.
	e.DELETE("/habits/:id", func(c echo.Context) error {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "id inválido")
		}
		if err := store.DeleteHabit(c.Request().Context(), id); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		// Estado já SEM o hábito (e sem o foco, se era nele que estava ativo).
		d, err := focusStatusPayload(c.Request().Context(), store)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		d.OOB = true // liga o hx-swap-oob no painel só nesta resposta

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
		c.Response().WriteHeader(http.StatusOK)
		if err := t.templates.ExecuteTemplate(c.Response(), "habit-select", d.Habits); err != nil {
			return err
		}
		return t.templates.ExecuteTemplate(c.Response(), "focus-status", d)
	})

	// POST /heartbeat — telemetria passiva do editor (Fase 4). O plugin coalesce
	// os eventos em memória e manda lotes pequenos em JSON; aqui só validamos o
	// envelope e gravamos o lote numa transação. Resposta em texto puro ("ok N"),
	// como todo endpoint consumido por cliente de terminal.
	e.POST("/heartbeat", func(c echo.Context) error {
		var p heartbeatPayload
		if err := c.Bind(&p); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "payload inválido")
		}
		if len(p.Heartbeats) > maxHeartbeatBatch {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "lote de heartbeats grande demais")
		}
		if p.Source == "" {
			p.Source = "unknown"
		}
		if err := store.InsertHeartbeats(c.Request().Context(), p.Source, p.Heartbeats); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.String(http.StatusOK, fmt.Sprintf("ok %d", len(p.Heartbeats)))
	})

	// POST /focus — registra uma sessão de foco JÁ CONCLUÍDA (log manual/script).
	e.POST("/focus", func(c echo.Context) error {
		habitID, err := strconv.ParseInt(c.FormValue("habit_id"), 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "habit_id inválido")
		}
		duration, err := strconv.Atoi(c.FormValue("duration_minutes"))
		if err != nil || duration <= 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "duration_minutes inválido")
		}

		if _, err := store.LogFocusSession(c.Request().Context(), habitID, duration); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		return c.Render(http.StatusOK, "focus-result", duration)
	})

	// POST /focus/start — marca o início de uma sessão ativa (Neovim/tmux).
	// Retorna o texto de status para o cliente exibir de imediato.
	e.POST("/focus/start", func(c echo.Context) error {
		habitID, err := strconv.ParseInt(c.FormValue("habit_id"), 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "habit_id inválido")
		}
		if err := store.StartFocus(c.Request().Context(), habitID); err != nil {
			if errors.Is(err, ErrFocusActive) {
				// Navegador: apenas reflete o estado ativo já existente (idempotente
				// na UI). Terminal: 409 com a mensagem.
				if isHTMX(c) {
					return renderFocusStatus(c, store)
				}
				return c.String(http.StatusConflict, err.Error())
			}
			if errors.Is(err, ErrHabitNotFound) {
				// Hábito não existe (ex.: keybind apontando para id deletado).
				// Navegador: re-renderiza o painel, descartando a opção obsoleta.
				if isHTMX(c) {
					return renderFocusStatus(c, store)
				}
				return c.String(http.StatusNotFound, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if isHTMX(c) {
			return renderFocusStatus(c, store)
		}
		active, err := store.GetActiveFocus(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.String(http.StatusOK, statusText(active))
	})

	// POST /focus/stop — encerra a sessão ativa, grava a duração e limpa o estado.
	e.POST("/focus/stop", func(c echo.Context) error {
		duration, habitID, err := store.StopFocus(c.Request().Context())
		if err != nil {
			if errors.Is(err, ErrNoActiveFocus) {
				if isHTMX(c) {
					return renderFocusStatus(c, store)
				}
				return c.String(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if isHTMX(c) {
			return renderFocusStatus(c, store)
		}
		return c.String(http.StatusOK, fmt.Sprintf("✓ %dm registrados (hábito %d)", duration, habitID))
	})

	// GET /status — endpoint ultra-leve em texto puro para a barra de status.
	// Ex.: "🎯 Foco: 15m" ou "Nenhum foco ativo".
	e.GET("/status", func(c echo.Context) error {
		active, err := store.GetActiveFocus(c.Request().Context())
		if err != nil {
			return c.String(http.StatusOK, "Nenhum foco ativo")
		}
		return c.String(http.StatusOK, statusText(active))
	})

	// GET /streak — dias locais consecutivos com atividade, como inteiro em
	// texto puro. Consumido pelo hook de commit para compor a mensagem.
	e.GET("/streak", func(c echo.Context) error {
		n, err := store.Streak(c.Request().Context())
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.String(http.StatusOK, strconv.Itoa(n))
	})

	// Graceful shutdown: SIGINT/SIGTERM (o `focusd stop` manda SIGTERM) drenam as
	// requisições em voo e devolvem o controle ao main, onde o defer store.Close()
	// fecha os statements e a conexão — o driver faz o checkpoint final do WAL.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Sentinela de time-jump (Fase 3): desconta hibernação/sleep da sessão
	// ativa. Vive amarrada ao ctx do shutdown — morre junto com o daemon.
	go watchClockJumps(ctx, store)

	// Listener PRIMÁRIO: Unix domain socket 0600. Substitui o TCP como canal
	// de IPC — zero colisão de porta, permissão via filesystem (só o dono fala).
	sock := socketPath()
	unixL, err := listenUnix(sock)
	if err != nil {
		log.Fatalf("socket unix: %v", err)
	}

	// Um único http.Server atende N listeners (socket sempre, TCP opcional).
	srv := &http.Server{Handler: e}

	go func() {
		if err := srv.Serve(unixL); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("socket serve: %v", err)
			stop()
		}
	}()

	// TCP OPCIONAL: só quando FOCUSD_ADDR está setado (ex.: abrir a UI web).
	if tcpAddr != "" {
		if tcpL, err := net.Listen("tcp", tcpAddr); err != nil {
			log.Printf("tcp %s: %v", tcpAddr, err)
		} else {
			go func() {
				if err := srv.Serve(tcpL); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("tcp serve: %v", err)
				}
			}()
			log.Printf("UI de navegador em http://%s", tcpAddr)
		}
	}

	log.Printf("focusd no ar · socket %s · db %s", sock, dbPath)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	_ = os.Remove(sock) // limpa o socket ao sair, sem deixar arquivo órfão
	log.Println("focusd encerrado")
}
