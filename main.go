package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
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
func focusStatusPayload(store *Store) (focusStatusData, error) {
	habits, err := store.GetHabits()
	if err != nil {
		return focusStatusData{}, err
	}
	active, err := store.GetActiveFocus()
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
	d, err := focusStatusPayload(store)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Render(http.StatusOK, "focus-status", d)
}

func main() {
	// Config por ambiente:
	//   FOCUSD_DB   — caminho do banco SQLite (default: focus.db no CWD)
	//   FOCUSD_ADDR — endereço de escuta (default: SOMENTE loopback — produto
	//                 local-first não expõe a API na rede; quem precisar expor
	//                 opta explicitamente via FOCUSD_ADDR)
	dbPath := env("FOCUSD_DB", "focus.db")
	addr := env("FOCUSD_ADDR", "127.0.0.1:8080")

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
	e.Renderer = t
	// Logger com Skipper: os endpoints de polling (tmux a cada 1s, lualine a
	// cada 5s, HTMX a cada 15s) gerariam ~110k linhas/dia de ruído num daemon
	// que roda para sempre. Loga só o que é mutação/página.
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/status" || c.Path() == "/focus/status"
		},
	}))
	e.Use(middleware.Recover())

	// GET / — renderiza a página completa com a lista atual de hábitos e o
	// estado de foco corrente.
	e.GET("/", func(c echo.Context) error {
		d, err := focusStatusPayload(store)
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
		habits, err := store.GetHabits()
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

		id, err := store.CreateHabit(name, c.FormValue("description"), frequency)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		// Estado completo pós-inserção (mesma receita do DELETE): além do item
		// novo, sincroniza via OOB tudo que depende da lista de hábitos.
		d, err := focusStatusPayload(store)
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
		if err := store.DeleteHabit(id); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}

		// Estado já SEM o hábito (e sem o foco, se era nele que estava ativo).
		d, err := focusStatusPayload(store)
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

		if _, err := store.LogFocusSession(habitID, duration); err != nil {
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
		if err := store.StartFocus(habitID); err != nil {
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
		active, err := store.GetActiveFocus()
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.String(http.StatusOK, statusText(active))
	})

	// POST /focus/stop — encerra a sessão ativa, grava a duração e limpa o estado.
	e.POST("/focus/stop", func(c echo.Context) error {
		duration, habitID, err := store.StopFocus()
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
		active, err := store.GetActiveFocus()
		if err != nil {
			return c.String(http.StatusOK, "Nenhum foco ativo")
		}
		return c.String(http.StatusOK, statusText(active))
	})

	// Graceful shutdown: SIGINT/SIGTERM (o `focusd stop` manda SIGTERM) drenam as
	// requisições em voo e devolvem o controle ao main, onde o defer store.Close()
	// fecha os statements e a conexão — o driver faz o checkpoint final do WAL.
	// (O log.Fatal antigo chamava os.Exit e PULAVA os defers.)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := e.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("servidor: %v", err)
			stop() // falha de bind também encerra o daemon de forma ordenada
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("focusd encerrado")
}
