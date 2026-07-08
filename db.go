package main

import (
	"database/sql"
	"errors"
	"fmt"
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

// ActiveFocus é a sessão de foco em andamento (no máximo uma, no servidor).
type ActiveFocus struct {
	HabitID   int64
	StartedAt time.Time
}

// MinutesElapsed retorna os minutos inteiros decorridos desde o início,
// nunca negativos — clock skew (NTP, fuso, relógio ajustado à mão) não pode
// fazer a barra de status exibir "🎯 Foco: -3m".
func (a *ActiveFocus) MinutesElapsed() int {
	if m := int(time.Since(a.StartedAt).Minutes()); m > 0 {
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
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	habit_id   INTEGER NOT NULL,
	started_at INTEGER NOT NULL, -- unix epoch (segundos), controlado pelo Go
	FOREIGN KEY (habit_id) REFERENCES habits(id) ON DELETE CASCADE
);

-- Índice para acelerar a busca de logs por hábito.
CREATE INDEX IF NOT EXISTS idx_focus_logs_habit_id ON focus_logs(habit_id);
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
}

// NewStore abre (ou cria) o banco, aplica PRAGMAs de performance, garante o
// schema e pré-compila os statements mais usados.
func NewStore(path string) (*Store, error) {
	// PRAGMAs via sintaxe do modernc (_pragma=nome(valor), aplicados a CADA
	// conexão do pool no momento em que é aberta):
	//   journal_mode(WAL) + synchronous(NORMAL) → escrita rápida com durabilidade;
	//   busy_timeout(5000) → espera até 5s por um lock em vez de estourar
	//                        "database is locked" na hora (a defesa central);
	//   foreign_keys(1)    → integridade referencial + ON DELETE CASCADE.
	// _txlock=immediate faz cada transação do database/sql nascer com
	// BEGIN IMMEDIATE (já como escritora), eliminando o SQLITE_BUSY_SNAPSHOT
	// no upgrade read→write do StopFocus caso um processo externo (ex.: o CLI
	// sqlite3 de debug) escreva no banco no meio do caminho.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"+
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
		{&s.stmtGetActiveFocus, `SELECT habit_id, started_at FROM active_focus WHERE id = 1`},
		{&s.stmtStartFocus, `INSERT INTO active_focus (id, habit_id, started_at) VALUES (1, ?, ?)`},
		{&s.stmtDeleteActive, `DELETE FROM active_focus WHERE id = 1`},
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
		s.stmtGetActiveFocus, s.stmtStartFocus, s.stmtDeleteActive,
	} {
		if stmt != nil {
			stmt.Close()
		}
	}
	return s.db.Close()
}

// CreateHabit insere um novo hábito e retorna seu ID gerado.
func (s *Store) CreateHabit(name, description, frequency string) (int64, error) {
	res, err := s.stmtInsertHabit.Exec(name, description, frequency)
	if err != nil {
		return 0, fmt.Errorf("inserindo hábito: %w", err)
	}
	return res.LastInsertId()
}

// GetHabits retorna todos os hábitos ordenados pelo mais recente.
func (s *Store) GetHabits() ([]Habit, error) {
	rows, err := s.stmtGetHabits.Query()
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
func (s *Store) DeleteHabit(id int64) error {
	if _, err := s.stmtDeleteHabit.Exec(id); err != nil {
		return fmt.Errorf("deletando hábito: %w", err)
	}
	return nil
}

// LogFocusSession registra uma sessão de foco (já concluída) para um hábito.
func (s *Store) LogFocusSession(habitID int64, durationMinutes int) (int64, error) {
	res, err := s.stmtInsertFocusLog.Exec(habitID, durationMinutes)
	if err != nil {
		return 0, fmt.Errorf("inserindo log de foco: %w", err)
	}
	return res.LastInsertId()
}

// GetActiveFocus retorna a sessão de foco em andamento, se houver.
func (s *Store) GetActiveFocus() (*ActiveFocus, error) {
	var habitID, startedAt int64
	err := s.stmtGetActiveFocus.QueryRow().Scan(&habitID, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consultando foco ativo: %w", err)
	}
	return &ActiveFocus{HabitID: habitID, StartedAt: time.Unix(startedAt, 0)}, nil
}

// StartFocus marca o início de uma sessão. Erro ErrFocusActive se já houver uma.
//
// Atômico por construção: em vez de "checar-depois-inserir" (que abre um TOCTOU
// sob concorrência — duas goroutines leem "vazio" e ambas tentam inserir), deixa
// o PRIMARY KEY CHECK(id = 1) da tabela active_focus ser a única fonte de verdade.
// O primeiro INSERT vence; qualquer INSERT concorrente viola a constraint e é
// traduzido para ErrFocusActive (409 limpo), nunca um 500. Sem duplo-ativo possível.
func (s *Store) StartFocus(habitID int64) error {
	if _, err := s.stmtStartFocus.Exec(habitID, time.Now().Unix()); err != nil {
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

// StopFocus encerra a sessão ativa: calcula a duração, grava em focus_logs e
// limpa o estado — tudo numa transação para não haver contagem dupla.
// Retorna a duração (min, mínimo 1) e o habit_id encerrado.
func (s *Store) StopFocus() (duration int, habitID int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("abrindo transação: %w", err)
	}
	defer tx.Rollback() // no-op após Commit bem-sucedido

	var startedAt int64
	err = tx.Stmt(s.stmtGetActiveFocus).QueryRow().Scan(&habitID, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNoActiveFocus
	}
	if err != nil {
		return 0, 0, fmt.Errorf("lendo foco ativo: %w", err)
	}

	elapsed := time.Since(time.Unix(startedAt, 0))
	duration = int(elapsed.Minutes())
	if duration < 1 {
		duration = 1 // não perde sessões curtas
	}

	if _, err = tx.Stmt(s.stmtInsertFocusLog).Exec(habitID, duration); err != nil {
		return 0, 0, fmt.Errorf("gravando log de foco: %w", err)
	}
	if _, err = tx.Stmt(s.stmtDeleteActive).Exec(); err != nil {
		return 0, 0, fmt.Errorf("limpando foco ativo: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return duration, habitID, nil
}
