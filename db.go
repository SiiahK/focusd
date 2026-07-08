package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// Driver SQLite PURO em Go (zero CGO). O import não-branco registra o
	// driver "sqlite" via init() E expõe o tipo *sqlite.Error usado abaixo.
	"modernc.org/sqlite"
	// Constantes de result-code do SQLite (SQLITE_CONSTRAINT_*), para
	// distinguir violação de FK de violação de PK/CHECK.
	sqlite3 "modernc.org/sqlite/lib"
)

// Erros sentinela do domínio de foco.
var (
	// ErrFocusActive é retornado ao tentar iniciar um foco com outro já em andamento.
	ErrFocusActive = errors.New("já existe uma sessão de foco ativa")
	// ErrNoActiveFocus é retornado ao tentar encerrar sem nenhuma sessão ativa.
	ErrNoActiveFocus = errors.New("nenhuma sessão de foco ativa")
	// ErrHabitNotFound é retornado ao iniciar foco para um habit_id inexistente
	// (ex.: keybind do tmux apontando para um hábito já deletado).
	ErrHabitNotFound = errors.New("hábito não encontrado")
)

// Habit representa um hábito rastreado pelo usuário.
type Habit struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Frequency   string    `json:"frequency"` // ex.: "daily", "weekly"
	CreatedAt   time.Time `json:"created_at"`
}

// FocusLog representa uma sessão de foco registrada para um hábito.
type FocusLog struct {
	ID              int64     `json:"id"`
	HabitID         int64     `json:"habit_id"`
	DurationMinutes int       `json:"duration_minutes"`
	LoggedAt        time.Time `json:"logged_at"`
}

// Heartbeat é um pacote de telemetria passiva já COALESCIDO pelo editor:
// "houve {Events} eventos de atividade em {File} a partir do instante {At}".
// As tags json são o contrato do POST /heartbeat com o focusd.nvim.
type Heartbeat struct {
	Project  string `json:"project"`
	File     string `json:"file"`
	Language string `json:"language"`
	Events   int64  `json:"events"`
	At       int64  `json:"at"` // unix epoch (s) do início do intervalo coalescido
}

// ActiveFocus é a sessão de foco em andamento (no máximo uma, no servidor).
type ActiveFocus struct {
	HabitID   int64
	StartedAt time.Time
	// SuspendedSecs acumula os segundos em que a máquina esteve suspensa
	// (hibernação/sleep) DURANTE a sessão, detectados pelo watchClockJumps
	// (clock.go). São descontados do tempo decorrido: acordar o laptop de
	// manhã não pode virar uma "sessão de foco" de 9 horas.
	SuspendedSecs int64
}

// MinutesElapsed retorna os minutos inteiros decorridos desde o início,
// descontado o tempo suspenso, e nunca negativos — clock skew (NTP, fuso,
// relógio ajustado à mão) não pode fazer a barra exibir "🎯 Foco: -3m".
func (a *ActiveFocus) MinutesElapsed() int {
	elapsed := time.Since(a.StartedAt) - time.Duration(a.SuspendedSecs)*time.Second
	if m := int(elapsed.Minutes()); m > 0 {
		return m
	}
	return 0
}

const schema = `
CREATE TABLE IF NOT EXISTS habits (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	frequency   TEXT NOT NULL DEFAULT 'daily',
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS focus_logs (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	habit_id         INTEGER NOT NULL,
	duration_minutes INTEGER NOT NULL,
	logged_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (habit_id) REFERENCES habits(id) ON DELETE CASCADE
);

-- Sessão de foco em andamento. CHECK(id = 1) garante no máximo uma linha:
-- é o "estado global" lido por /status, lualine e tmux.
CREATE TABLE IF NOT EXISTS active_focus (
	id             INTEGER PRIMARY KEY CHECK (id = 1),
	habit_id       INTEGER NOT NULL,
	started_at     INTEGER NOT NULL, -- unix epoch (segundos), controlado pelo Go
	suspended_secs INTEGER NOT NULL DEFAULT 0, -- segundos suspensos, descontados do decorrido
	FOREIGN KEY (habit_id) REFERENCES habits(id) ON DELETE CASCADE
);

-- Índice para acelerar a busca de logs por hábito.
CREATE INDEX IF NOT EXISTS idx_focus_logs_habit_id ON focus_logs(habit_id);

-- Telemetria passiva do editor (Fase 4). Uma linha por (arquivo × intervalo
-- coalescido); quem limita o volume é o coalescing client-side do plugin,
-- não o banco. Consultas típicas são por janela de tempo, daí o índice em at.
CREATE TABLE IF NOT EXISTS heartbeats (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	source   TEXT NOT NULL,
	project  TEXT NOT NULL DEFAULT '',
	file     TEXT NOT NULL DEFAULT '',
	language TEXT NOT NULL DEFAULT '',
	events   INTEGER NOT NULL DEFAULT 1,
	at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_heartbeats_at ON heartbeats(at);
`

// Store encapsula a conexão SQLite e os statements pré-compilados (db.Prepare).
// Preparar uma única vez os statements do caminho quente evita reparsear o SQL
// a cada request. (Ganho real neste volume é marginal — o benefício maior é ter
// um único ponto de verdade para as queries; ver nota no README/entrega.)
type Store struct {
	db *sql.DB

	stmtInsertHabit    *sql.Stmt
	stmtGetHabits      *sql.Stmt
	stmtDeleteHabit    *sql.Stmt
	stmtInsertFocusLog *sql.Stmt
	stmtGetActiveFocus *sql.Stmt
	stmtStartFocus     *sql.Stmt
	stmtDeleteActive   *sql.Stmt
	stmtAddSuspended   *sql.Stmt
	stmtInsertHB       *sql.Stmt
}

// NewStore abre (ou cria) o banco, aplica PRAGMAs de performance, garante o
// schema e pré-compila os statements mais usados.
func NewStore(path string) (*Store, error) {
	// PRAGMAs via sintaxe do modernc (_pragma=nome(valor), aplicados a CADA
	// conexão do pool no momento em que é aberta):
	//   journal_mode(WAL) + synchronous(NORMAL) → escrita rápida com durabilidade;
	//   busy_timeout(1500) → espera até 1.5s por um lock em vez de estourar
	//                        "database is locked" na hora (a defesa central).
	//                        DEVE caber dentro do requestTimeout (2s): o
	//                        sqlite3_interrupt disparado pelo ctx NÃO encurta
	//                        o busy-wait (verificado empiricamente no modernc
	//                        v1.53), então é o busy_timeout que limita quanto
	//                        tempo um lock externo segura a única conexão;
	//   foreign_keys(1)    → integridade referencial + ON DELETE CASCADE.
	// _txlock=immediate faz cada transação do database/sql nascer com
	// BEGIN IMMEDIATE (já como escritora), eliminando o SQLITE_BUSY_SNAPSHOT
	// no upgrade read→write do StopFocus caso um processo externo (ex.: o CLI
	// sqlite3 de debug) escreva no banco no meio do caminho.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(1500)"+
			"&_txlock=immediate",
		path,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrindo banco: %w", err)
	}

	// UMA única conexão: serializa TODO acesso ao banco. Além de o SQLite
	// embutido lidar melhor com um único escritor, isto torna impossível o
	// deadlock de upgrade read→write DENTRO do nosso processo (nunca há um
	// leitor concorrente segurando snapshot enquanto tentamos escrever).
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping no banco: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("aplicando schema: %w", err)
	}

	// Migração aditiva (Fase 3) para bancos criados antes de suspended_secs:
	// CREATE TABLE IF NOT EXISTS não altera tabela existente, então o ALTER
	// cobre o upgrade. "duplicate column name" = coluna já existe, não é erro.
	if _, err := db.Exec(
		`ALTER TABLE active_focus ADD COLUMN suspended_secs INTEGER NOT NULL DEFAULT 0`,
	); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		db.Close()
		return nil, fmt.Errorf("migrando active_focus: %w", err)
	}

	s := &Store{db: db}
	if err := s.prepare(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// prepare pré-compila os statements do caminho quente.
func (s *Store) prepare() error {
	stmts := []struct {
		dst   **sql.Stmt
		query string
	}{
		{&s.stmtInsertHabit, `INSERT INTO habits (name, description, frequency) VALUES (?, ?, ?)`},
		{&s.stmtGetHabits, `SELECT id, name, description, frequency, created_at FROM habits ORDER BY created_at DESC`},
		{&s.stmtDeleteHabit, `DELETE FROM habits WHERE id = ?`},
		{&s.stmtInsertFocusLog, `INSERT INTO focus_logs (habit_id, duration_minutes) VALUES (?, ?)`},
		{&s.stmtGetActiveFocus, `SELECT habit_id, started_at, suspended_secs FROM active_focus WHERE id = 1`},
		{&s.stmtStartFocus, `INSERT INTO active_focus (id, habit_id, started_at) VALUES (1, ?, ?)`},
		{&s.stmtDeleteActive, `DELETE FROM active_focus WHERE id = 1`},
		// O predicado started_at <= ? fecha a corrida do wake: se a sessão
		// nasceu DEPOIS do último tick do watchClockJumps, a suspensão detectada
		// aconteceu antes dela começar e não deve ser descontada.
		{&s.stmtAddSuspended, `UPDATE active_focus SET suspended_secs = suspended_secs + ? WHERE id = 1 AND started_at <= ?`},
		{&s.stmtInsertHB, `INSERT INTO heartbeats (source, project, file, language, events, at) VALUES (?, ?, ?, ?, ?, ?)`},
	}
	for _, st := range stmts {
		stmt, err := s.db.Prepare(st.query)
		if err != nil {
			return fmt.Errorf("preparando statement %q: %w", st.query, err)
		}
		*st.dst = stmt
	}
	return nil
}

// Close fecha os statements e a conexão.
func (s *Store) Close() error {
	for _, stmt := range []*sql.Stmt{
		s.stmtInsertHabit, s.stmtGetHabits, s.stmtDeleteHabit, s.stmtInsertFocusLog,
		s.stmtGetActiveFocus, s.stmtStartFocus, s.stmtDeleteActive, s.stmtAddSuspended,
		s.stmtInsertHB,
	} {
		if stmt != nil {
			stmt.Close()
		}
	}
	return s.db.Close()
}

// CreateHabit insere um novo hábito e retorna seu ID gerado.
func (s *Store) CreateHabit(ctx context.Context, name, description, frequency string) (int64, error) {
	res, err := s.stmtInsertHabit.ExecContext(ctx, name, description, frequency)
	if err != nil {
		return 0, fmt.Errorf("inserindo hábito: %w", err)
	}
	return res.LastInsertId()
}

// GetHabits retorna todos os hábitos ordenados pelo mais recente.
func (s *Store) GetHabits(ctx context.Context) ([]Habit, error) {
	rows, err := s.stmtGetHabits.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("consultando hábitos: %w", err)
	}
	defer rows.Close()

	habits := make([]Habit, 0)
	for rows.Next() {
		var h Habit
		if err := rows.Scan(&h.ID, &h.Name, &h.Description, &h.Frequency, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("lendo hábito: %w", err)
		}
		habits = append(habits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterando hábitos: %w", err)
	}
	return habits, nil
}

// DeleteHabit remove um hábito pelo ID. Com _foreign_keys=on, o ON DELETE
// CASCADE apaga em conjunto os focus_logs e um eventual active_focus vinculados.
// Deletar um ID inexistente é no-op (não é tratado como erro).
func (s *Store) DeleteHabit(ctx context.Context, id int64) error {
	if _, err := s.stmtDeleteHabit.ExecContext(ctx, id); err != nil {
		return fmt.Errorf("deletando hábito: %w", err)
	}
	return nil
}

// LogFocusSession registra uma sessão de foco (já concluída) para um hábito.
func (s *Store) LogFocusSession(ctx context.Context, habitID int64, durationMinutes int) (int64, error) {
	res, err := s.stmtInsertFocusLog.ExecContext(ctx, habitID, durationMinutes)
	if err != nil {
		return 0, fmt.Errorf("inserindo log de foco: %w", err)
	}
	return res.LastInsertId()
}

// GetActiveFocus retorna a sessão de foco em andamento, se houver.
func (s *Store) GetActiveFocus(ctx context.Context) (*ActiveFocus, error) {
	var habitID, startedAt, suspended int64
	err := s.stmtGetActiveFocus.QueryRowContext(ctx).Scan(&habitID, &startedAt, &suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consultando foco ativo: %w", err)
	}
	return &ActiveFocus{
		HabitID:       habitID,
		StartedAt:     time.Unix(startedAt, 0),
		SuspendedSecs: suspended,
	}, nil
}

// AddSuspendedSecs credita segundos de suspensão na sessão ativa, se ela já
// existia em startedAtMax (unix epoch — o instante do tick anterior do
// detector). Retorna quantas linhas foram afetadas: 0 significa "sem sessão
// ativa" ou "sessão mais nova que o salto detectado" — em ambos os casos não
// há o que descontar, e não é erro.
func (s *Store) AddSuspendedSecs(ctx context.Context, secs, startedAtMax int64) (int64, error) {
	res, err := s.stmtAddSuspended.ExecContext(ctx, secs, startedAtMax)
	if err != nil {
		return 0, fmt.Errorf("creditando suspensão: %w", err)
	}
	return res.RowsAffected()
}

// Streak conta os dias LOCAIS consecutivos com atividade (heartbeats ou
// sessões de foco), terminando hoje — ou ontem, se hoje ainda não teve
// atividade: um streak não morre à meia-noite, morre ao pular um dia.
// Fora do caminho quente (só o hook de commit chama), então sem stmt preparado.
func (s *Store) Streak(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT date(at, 'unixepoch', 'localtime') FROM heartbeats
		UNION
		SELECT DISTINCT date(logged_at, 'localtime') FROM focus_logs`)
	if err != nil {
		return 0, fmt.Errorf("consultando dias ativos: %w", err)
	}
	defer rows.Close()

	days := make(map[string]bool)
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return 0, fmt.Errorf("lendo dia ativo: %w", err)
		}
		days[d] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterando dias ativos: %w", err)
	}

	const layout = "2006-01-02"
	day := time.Now()
	if !days[day.Format(layout)] {
		day = day.AddDate(0, 0, -1)
	}
	n := 0
	for days[day.Format(layout)] {
		n++
		day = day.AddDate(0, 0, -1)
	}
	return n, nil
}

// clip limita campos de texto vindos da rede a um tamanho são — heartbeat é
// telemetria, não lugar para alguém estacionar um path de 1MB no banco.
func clip(v string) string {
	if len(v) > 512 {
		return v[:512]
	}
	return v
}

// InsertHeartbeats grava um lote de heartbeats numa ÚNICA transação: um fsync
// do WAL para o lote inteiro, não por linha. Normaliza em vez de rejeitar
// (events<1 vira 1; at ausente/futuro vira agora): telemetria passiva nunca
// deve virar erro visível dentro do editor do usuário.
func (s *Store) InsertHeartbeats(ctx context.Context, source string, hbs []Heartbeat) error {
	if len(hbs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abrindo transação: %w", err)
	}
	defer tx.Rollback() // no-op após Commit bem-sucedido

	now := time.Now().Unix()
	stmt := tx.StmtContext(ctx, s.stmtInsertHB)
	for _, hb := range hbs {
		if hb.Events < 1 {
			hb.Events = 1
		}
		if hb.At <= 0 || hb.At > now {
			hb.At = now
		}
		if _, err := stmt.ExecContext(ctx,
			clip(source), clip(hb.Project), clip(hb.File), clip(hb.Language), hb.Events, hb.At,
		); err != nil {
			return fmt.Errorf("gravando heartbeat: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit heartbeats: %w", err)
	}
	return nil
}

// StartFocus marca o início de uma sessão. Erro ErrFocusActive se já houver uma.
//
// Atômico por construção: em vez de "checar-depois-inserir" (que abre um TOCTOU
// sob concorrência — duas goroutines leem "vazio" e ambas tentam inserir), deixa
// o PRIMARY KEY CHECK(id = 1) da tabela active_focus ser a única fonte de verdade.
// O primeiro INSERT vence; qualquer INSERT concorrente viola a constraint e é
// traduzido para ErrFocusActive (409 limpo), nunca um 500. Sem duplo-ativo possível.
func (s *Store) StartFocus(ctx context.Context, habitID int64) error {
	if _, err := s.stmtStartFocus.ExecContext(ctx, habitID, time.Now().Unix()); err != nil {
		var se *sqlite.Error
		// Code() do modernc devolve o result-code ESTENDIDO; o byte baixo é o
		// primary code. `code & 0xff == SQLITE_CONSTRAINT` casa qualquer
		// violação de constraint, e o código estendido conta QUAL:
		if errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_CONSTRAINT {
			// FK violada = "esse hábito não existe" (404), não "já há foco" (409).
			if se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
				return ErrHabitNotFound
			}
			return ErrFocusActive // PK/CHECK(id=1): perdeu a corrida de INSERT
		}
		return fmt.Errorf("iniciando foco: %w", err)
	}
	return nil
}

// StopFocus encerra a sessão ativa: calcula a duração (já descontado o tempo
// suspenso), grava em focus_logs e limpa o estado — tudo numa transação para
// não haver contagem dupla. Retorna a duração (min, mínimo 1) e o habit_id.
func (s *Store) StopFocus(ctx context.Context) (duration int, habitID int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("abrindo transação: %w", err)
	}
	defer tx.Rollback() // no-op após Commit bem-sucedido

	var startedAt, suspended int64
	err = tx.StmtContext(ctx, s.stmtGetActiveFocus).QueryRowContext(ctx).Scan(&habitID, &startedAt, &suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNoActiveFocus
	}
	if err != nil {
		return 0, 0, fmt.Errorf("lendo foco ativo: %w", err)
	}

	elapsed := time.Since(time.Unix(startedAt, 0)) - time.Duration(suspended)*time.Second
	duration = int(elapsed.Minutes())
	if duration < 1 {
		duration = 1 // não perde sessões curtas
	}

	if _, err = tx.StmtContext(ctx, s.stmtInsertFocusLog).ExecContext(ctx, habitID, duration); err != nil {
		return 0, 0, fmt.Errorf("gravando log de foco: %w", err)
	}
	if _, err = tx.StmtContext(ctx, s.stmtDeleteActive).ExecContext(ctx); err != nil {
		return 0, 0, fmt.Errorf("limpando foco ativo: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return duration, habitID, nil
}
