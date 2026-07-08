package main

// tui.go — Fase 7: `focusd tui`, o dashboard interativo (bubbletea).
//
// O TUI é um CLIENTE como qualquer outro: fala com o daemon pelo Unix socket
// (nunca abre o SQLite direto — a única conexão é do daemon) e busca um frame
// completo em um único GET /api/summary a cada 2s. As teclas de start/stop
// batem nos mesmos endpoints do tmux e do navegador.
//
// Três abas:
//   [1] dashboard — streak, sessão ao vivo, totais e o gráfico diário
//   [2] projects  — o report projeto × linguagem, janela de 7/14/30 dias
//   [3] sessions  — hábitos navegáveis; enter inicia/encerra foco AO VIVO

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const tuiRefresh = 2 * time.Second

// ── estilos ────────────────────────────────────────────────────────────────

var (
	stAccent   = lipgloss.NewStyle().Foreground(lipgloss.Color("212")) // rosa
	stGood     = lipgloss.NewStyle().Foreground(lipgloss.Color("120")) // verde
	stDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	stBar      = lipgloss.NewStyle().Foreground(lipgloss.Color("183"))
	stTab      = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("243"))
	stTabOn    = lipgloss.NewStyle().Padding(0, 2).Bold(true).Foreground(lipgloss.Color("212")).Underline(true)
	stStatCard = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 2)
	stHeader   = lipgloss.NewStyle().Bold(true)
	stCursor   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	stErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// ── mensagens ──────────────────────────────────────────────────────────────

type summaryMsg struct{ s *apiSummary }
type tuiErrMsg struct{ err error }
type tuiTickMsg struct{}
type actionMsg struct{ note string }

// ── modelo ─────────────────────────────────────────────────────────────────

type tuiModel struct {
	client  *http.Client
	tab     int // 0 dashboard · 1 projects · 2 sessions
	days    int // janela do report/gráfico (7/14/30)
	summary *apiSummary
	err     error
	note    string
	cursor  int // seleção na aba sessions
	width   int
}

func newTUIModel(client *http.Client) tuiModel {
	return tuiModel{client: client, days: 14, width: 80}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.fetch(), tuiTick())
}

func tuiTick() tea.Cmd {
	return tea.Tick(tuiRefresh, func(time.Time) tea.Msg { return tuiTickMsg{} })
}

// fetch busca um frame novo do daemon.
func (m tuiModel) fetch() tea.Cmd {
	client, days := m.client, m.days
	return func() tea.Msg {
		resp, err := client.Get(fmt.Sprintf("http://focusd/api/summary?days=%d", days))
		if err != nil {
			return tuiErrMsg{err}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return tuiErrMsg{fmt.Errorf("daemon respondeu %s", resp.Status)}
		}
		var s apiSummary
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return tuiErrMsg{err}
		}
		return summaryMsg{&s}
	}
}

// toggleFocus inicia foco no hábito selecionado, ou encerra a sessão ativa.
func (m tuiModel) toggleFocus() tea.Cmd {
	if m.summary == nil {
		return nil
	}
	client := m.client
	if a := m.summary.Active; a != nil {
		return func() tea.Msg {
			resp, err := client.PostForm("http://focusd/focus/stop", url.Values{})
			if err != nil {
				return tuiErrMsg{err}
			}
			resp.Body.Close()
			return actionMsg{"sessão encerrada e registrada"}
		}
	}
	if len(m.summary.Habits) == 0 {
		return func() tea.Msg { return actionMsg{"crie um hábito primeiro (tecla n)"} }
	}
	id := m.summary.Habits[m.cursor].ID
	return func() tea.Msg {
		resp, err := client.PostForm("http://focusd/focus/start",
			url.Values{"habit_id": {fmt.Sprint(id)}})
		if err != nil {
			return tuiErrMsg{err}
		}
		resp.Body.Close()
		return actionMsg{"foco iniciado"}
	}
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tuiTickMsg:
		return m, tea.Batch(m.fetch(), tuiTick())

	case summaryMsg:
		m.summary, m.err = msg.s, nil
		if n := len(msg.s.Habits); n > 0 && m.cursor >= n {
			m.cursor = n - 1
		}
		return m, nil

	case tuiErrMsg:
		m.err = msg.err
		return m, nil

	case actionMsg:
		m.note = msg.note
		return m, m.fetch()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab", "l", "right":
			m.tab = (m.tab + 1) % 3
		case "shift+tab", "h", "left":
			m.tab = (m.tab + 2) % 3
		case "1":
			m.tab = 0
		case "2":
			m.tab = 1
		case "3":
			m.tab = 2
		case "d":
			switch m.days {
			case 7:
				m.days = 14
			case 14:
				m.days = 30
			default:
				m.days = 7
			}
			return m, m.fetch()
		case "j", "down":
			if m.summary != nil && m.cursor < len(m.summary.Habits)-1 {
				m.cursor++
			}
		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}
		case "enter", " ":
			if m.tab == 2 {
				return m, m.toggleFocus()
			}
		case "r":
			return m, m.fetch()
		}
	}
	return m, nil
}

// ── views ──────────────────────────────────────────────────────────────────

func (m tuiModel) View() string {
	var b strings.Builder

	// abas
	names := []string{"dashboard", "projects", "sessions"}
	tabs := make([]string, len(names))
	for i, n := range names {
		label := fmt.Sprintf("%d·%s", i+1, n)
		if i == m.tab {
			tabs[i] = stTabOn.Render(label)
		} else {
			tabs[i] = stTab.Render(label)
		}
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, tabs...))
	b.WriteString("\n\n")

	switch {
	case m.summary == nil && m.err != nil:
		b.WriteString(stErr.Render("daemon fora do ar: " + m.err.Error()))
	case m.summary == nil:
		b.WriteString(stDim.Render("carregando…"))
	case m.tab == 0:
		b.WriteString(m.viewDashboard())
	case m.tab == 1:
		b.WriteString(m.viewProjects())
	default:
		b.WriteString(m.viewSessions())
	}

	// rodapé
	b.WriteString("\n\n")
	help := "tab/1-3: abas · d: 7/14/30d · j/k: mover · enter: foco on/off · r: atualizar · q: sair"
	b.WriteString(stDim.Render(help))
	if m.err != nil && m.summary != nil {
		b.WriteString("\n" + stErr.Render("⚠ "+m.err.Error()))
	} else if m.note != "" {
		b.WriteString("\n" + stGood.Render("✓ "+m.note))
	}
	return b.String()
}

func (m tuiModel) viewDashboard() string {
	s := m.summary
	var total int64
	for _, r := range s.Report {
		total += r.Minutes
	}
	today := int64(0)
	todayKey := time.Now().Format("2006-01-02")
	for _, d := range s.Daily {
		if d.Date == todayKey {
			today = d.Minutes
		}
	}

	card := func(title, value string) string {
		return stStatCard.Render(stDim.Render(title) + "\n" + stHeader.Render(value))
	}
	cards := lipgloss.JoinHorizontal(lipgloss.Top,
		card("STREAK", stAccent.Render(fmt.Sprintf("🔥 %d", s.Streak))+stDim.Render(" dias")),
		card("HOJE", fmt.Sprintf("%dm", today)),
		card(fmt.Sprintf("%d DIAS", s.Days), fmt.Sprintf("%dm", total)),
		card("FOCO", fmt.Sprintf("%dm · %d sessões", s.FocusMinutes, s.FocusSessions)),
	)

	live := stDim.Render("○ nenhuma sessão ativa — aba 3 para iniciar")
	if a := s.Active; a != nil {
		live = stGood.Render(fmt.Sprintf("● %s — %dm ao vivo", a.HabitName, a.Minutes))
	}

	return cards + "\n\n " + live + "\n\n" + m.viewDailyChart()
}

// viewDailyChart preenche as lacunas da série (dias sem atividade) e desenha
// uma barra proporcional por dia, mais recente embaixo.
func (m tuiModel) viewDailyChart() string {
	s := m.summary
	byDay := make(map[string]int64, len(s.Daily))
	var max int64 = 1
	for _, d := range s.Daily {
		byDay[d.Date] = d.Minutes
		if d.Minutes > max {
			max = d.Minutes
		}
	}
	width := m.width - 24
	if width < 10 {
		width = 10
	} else if width > 48 {
		width = 48
	}

	var b strings.Builder
	b.WriteString(" " + stHeader.Render(fmt.Sprintf("ATIVIDADE · últimos %d dias", s.Days)) + "\n")
	for i := s.Days - 1; i >= 0; i-- {
		day := time.Now().AddDate(0, 0, -i)
		mins := byDay[day.Format("2006-01-02")]
		label := day.Format("Mon 02")
		if i == 0 {
			label = "hoje  "
		}
		fmt.Fprintf(&b, " %s  %4dm  %s\n",
			stDim.Render(label), mins, stBar.Render(barra(mins, max, width)))
	}
	return b.String()
}

func (m tuiModel) viewProjects() string {
	s := m.summary
	if len(s.Report) == 0 {
		return stDim.Render(" sem atividade na janela — o código conta sozinho enquanto você trabalha.")
	}
	pw, lw := len("PROJECT"), len("LANGUAGE")
	var max int64
	for _, r := range s.Report {
		if len(r.Project) > pw {
			pw = len(r.Project)
		}
		if len(r.Language) > lw {
			lw = len(r.Language)
		}
		if r.Minutes > max {
			max = r.Minutes
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, " %s\n", stHeader.Render(fmt.Sprintf("%-*s  %-*s  %6s", pw, "PROJECT", lw, "LANGUAGE", "ACTIVE")))
	for _, r := range s.Report {
		fmt.Fprintf(&b, " %-*s  %-*s  %5dm  %s\n",
			pw, r.Project, lw, r.Language, r.Minutes, stBar.Render(barra(r.Minutes, max, 20)))
	}
	return b.String()
}

func (m tuiModel) viewSessions() string {
	s := m.summary
	var b strings.Builder
	if a := s.Active; a != nil {
		b.WriteString(" " + stGood.Render(fmt.Sprintf("● %s — %dm ao vivo", a.HabitName, a.Minutes)) +
			stDim.Render("  (enter encerra e registra)") + "\n\n")
	} else {
		b.WriteString(" " + stDim.Render("○ ocioso — selecione um hábito e aperte enter") + "\n\n")
	}
	if len(s.Habits) == 0 {
		b.WriteString(stDim.Render(" nenhum hábito ainda — crie um na UI web (FOCUSD_ADDR=127.0.0.1:8080 focusd)"))
		return b.String()
	}
	for i, h := range s.Habits {
		cursor, style := "  ", lipgloss.NewStyle()
		if i == m.cursor {
			cursor, style = stCursor.Render("❯ "), stCursor
		}
		mark := " "
		if a := s.Active; a != nil && a.HabitID == h.ID {
			mark = stGood.Render("●")
		}
		fmt.Fprintf(&b, " %s%s %s\n", cursor, mark, style.Render(h.Name))
	}
	return b.String()
}
