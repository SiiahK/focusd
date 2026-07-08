package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestStore cria um Store num banco temporário isolado por teste.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestErrorMapping é o teste CRÍTICO da migração: o roteamento 404/409 do
// daemon depende de distinguir violação de FK (hábito inexistente) de
// violação de PK/CHECK (foco já ativo). Se o modernc devolvesse só o
// primary code, os dois colapsariam em ErrFocusActive e este teste pega.
func TestErrorMapping(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// 1) foco em hábito inexistente → FK violada → ErrHabitNotFound (404).
	if err := s.StartFocus(ctx, 999); !errors.Is(err, ErrHabitNotFound) {
		t.Fatalf("habit_id inexistente: esperava ErrHabitNotFound, veio %v", err)
	}

	// 2) cria hábito e inicia foco → ok.
	id, err := s.CreateHabit(ctx, "Codar", "", "daily")
	if err != nil {
		t.Fatalf("CreateHabit: %v", err)
	}
	if err := s.StartFocus(ctx, id); err != nil {
		t.Fatalf("StartFocus válido: %v", err)
	}

	// 3) segundo start com foco ativo → PK/CHECK(id=1) → ErrFocusActive (409).
	if err := s.StartFocus(ctx, id); !errors.Is(err, ErrFocusActive) {
		t.Fatalf("foco duplo: esperava ErrFocusActive, veio %v", err)
	}

	// 4) stop grava a sessão (mínimo 1 min) e limpa o estado.
	dur, hID, err := s.StopFocus(ctx)
	if err != nil {
		t.Fatalf("StopFocus: %v", err)
	}
	if dur < 1 || hID != id {
		t.Fatalf("StopFocus devolveu dur=%d habit=%d (esperava dur>=1 habit=%d)", dur, hID, id)
	}

	// 5) stop sem foco ativo → ErrNoActiveFocus (409).
	if _, _, err := s.StopFocus(ctx); !errors.Is(err, ErrNoActiveFocus) {
		t.Fatalf("stop ocioso: esperava ErrNoActiveFocus, veio %v", err)
	}
}

// TestSuspendDeduction cobre a Fase 3: segundos de suspensão creditados em
// suspended_secs são descontados tanto do MinutesElapsed (barra de status)
// quanto da duração gravada pelo StopFocus — e o desconto NÃO se aplica a
// sessões iniciadas depois do salto detectado (guarda started_at <= ?).
func TestSuspendDeduction(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.CreateHabit(ctx, "Codar", "", "daily")
	if err != nil {
		t.Fatalf("CreateHabit: %v", err)
	}
	if err := s.StartFocus(ctx, id); err != nil {
		t.Fatalf("StartFocus: %v", err)
	}

	// Simula: a sessão começou há 10 min (wall), mas 9 deles foram hibernação.
	if _, err := s.db.Exec(`UPDATE active_focus SET started_at = started_at - 600`); err != nil {
		t.Fatalf("recuando started_at: %v", err)
	}
	n, err := s.AddSuspendedSecs(ctx, 540, time.Now().Unix())
	if err != nil {
		t.Fatalf("AddSuspendedSecs: %v", err)
	}
	if n != 1 {
		t.Fatalf("AddSuspendedSecs afetou %d linhas (esperava 1)", n)
	}

	a, err := s.GetActiveFocus(ctx)
	if err != nil {
		t.Fatalf("GetActiveFocus: %v", err)
	}
	if a == nil || a.MinutesElapsed() != 1 {
		t.Fatalf("MinutesElapsed=%v (esperava 1: 10min wall − 9min suspenso)", a)
	}

	dur, _, err := s.StopFocus(ctx)
	if err != nil {
		t.Fatalf("StopFocus: %v", err)
	}
	if dur != 1 {
		t.Fatalf("StopFocus dur=%d (esperava 1: 10min wall − 9min suspenso)", dur)
	}

	// Guarda anti-corrida do wake: um salto detectado ANTES da sessão nascer
	// (startedAtMax no passado) não desconta nada da sessão nova.
	if err := s.StartFocus(ctx, id); err != nil {
		t.Fatalf("StartFocus 2: %v", err)
	}
	n, err = s.AddSuspendedSecs(ctx, 300, time.Now().Unix()-3600)
	if err != nil {
		t.Fatalf("AddSuspendedSecs 2: %v", err)
	}
	if n != 0 {
		t.Fatalf("desconto vazou para sessão nova: %d linhas afetadas (esperava 0)", n)
	}
}

// TestInsertHeartbeats cobre o batch-insert da Fase 4: transação única,
// normalização de campos hostis (events<1, at futuro, strings gigantes) e
// no-op para lote vazio.
func TestInsertHeartbeats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.InsertHeartbeats(ctx, "nvim", nil); err != nil {
		t.Fatalf("lote vazio deveria ser no-op: %v", err)
	}

	long := make([]byte, 2048)
	for i := range long {
		long[i] = 'a'
	}
	hbs := []Heartbeat{
		{Project: "focusd", File: "main.go", Language: "go", Events: 7, At: time.Now().Unix() - 30},
		{File: string(long), Events: 0, At: time.Now().Unix() + 999999}, // hostil: normaliza
	}
	if err := s.InsertHeartbeats(ctx, "nvim", hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	var n, badEvents, futureAt, longFiles int
	row := s.db.QueryRow(`SELECT COUNT(*),
		SUM(events < 1), SUM(at > strftime('%s','now') + 5), SUM(length(file) > 512)
		FROM heartbeats`)
	if err := row.Scan(&n, &badEvents, &futureAt, &longFiles); err != nil {
		t.Fatalf("verificando heartbeats: %v", err)
	}
	if n != 2 || badEvents != 0 || futureAt != 0 || longFiles != 0 {
		t.Fatalf("normalização falhou: n=%d badEvents=%d futureAt=%d longFiles=%d", n, badEvents, futureAt, longFiles)
	}
}

// TestConcurrencyNoLock martela o banco de várias goroutines para provar que
// SetMaxOpenConns(1) + busy_timeout eliminam o "database is locked".
func TestConcurrencyNoLock(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	id, err := s.CreateHabit(ctx, "Codar", "", "daily")
	if err != nil {
		t.Fatalf("CreateHabit: %v", err)
	}

	const workers = 50
	var wg sync.WaitGroup
	errCh := make(chan error, workers*4)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Cada worker faz um ciclo completo + leituras. As corridas de
			// StartFocus são resolvidas pela constraint (ErrFocusActive é
			// esperado, NÃO é falha); o que NÃO pode aparecer é "locked".
			_ = s.StartFocus(ctx, id)
			if _, err := s.GetActiveFocus(ctx); err != nil {
				errCh <- err
			}
			if _, err := s.GetHabits(ctx); err != nil {
				errCh <- err
			}
			if _, _, err := s.StopFocus(ctx); err != nil && !errors.Is(err, ErrNoActiveFocus) {
				errCh <- err
			}
			if _, err := s.LogFocusSession(ctx, id, 1); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("erro sob concorrência (locked?): %v", err)
	}
}

// BenchmarkLogFocusSession mede o caminho de escrita quente (o "voando").
func BenchmarkLogFocusSession(b *testing.B) {
	ctx := context.Background()
	path := filepath.Join(b.TempDir(), "bench.db")
	s, err := NewStore(path)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	id, _ := s.CreateHabit(ctx, "Codar", "", "daily")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.LogFocusSession(ctx, id, 1); err != nil {
			b.Fatalf("LogFocusSession: %v", err)
		}
	}
}
