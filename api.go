package main

// api.go — Fase 7: o contrato JSON entre o daemon e o `focusd tui`.
//
// Um ÚNICO endpoint (GET /api/summary) entrega tudo que o dashboard precisa
// por frame: streak, sessão ativa, report agregado, atividade diária e a
// lista de hábitos. Um round-trip por refresh — o TUI nunca faz fan-out de
// requests contra a única conexão SQLite do daemon.
//
// Os tipos vivem neste arquivo porque são compartilhados pelos dois lados
// (handler no main.go, cliente no tui.go — mesmo pacote, mesmo binário).

// apiActive é a sessão de foco em andamento, já resolvida com nome.
type apiActive struct {
	HabitID   int64  `json:"habit_id"`
	HabitName string `json:"habit_name"`
	Minutes   int    `json:"minutes"`
}

// apiHabit é a projeção mínima de um hábito para o TUI.
type apiHabit struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// apiDaily é um dia de atividade agregada (minutos ativos).
type apiDaily struct {
	Date    string `json:"date"` // YYYY-MM-DD, fuso local do daemon
	Minutes int64  `json:"minutes"`
}

// apiSummary é o payload completo de um frame do dashboard.
type apiSummary struct {
	Days          int         `json:"days"`
	Streak        int         `json:"streak"`
	Active        *apiActive  `json:"active"` // null quando ocioso
	Report        []ReportRow `json:"report"`
	Daily         []apiDaily  `json:"daily"`
	FocusMinutes  int64       `json:"focus_minutes"`
	FocusSessions int64       `json:"focus_sessions"`
	Habits        []apiHabit  `json:"habits"`
}
