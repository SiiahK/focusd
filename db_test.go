package main

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
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
	s := newTestStore(t)

	// 1) foco em hábito inexistente → FK violada → ErrHabitNotFound (404).
	if err := s.StartFocus(999); !errors.Is(err, ErrHabitNotFound) {
		t.Fatalf("habit_id inexistente: esperava ErrHabitNotFound, veio %v", err)
	}

	// 2) cria hábito e inicia foco → ok.
	id, err := s.CreateHabit("Codar", "", "daily")
	if err != nil {
		t.Fatalf("CreateHabit: %v", err)
	}
	if err := s.StartFocus(id); err != nil {
		t.Fatalf("StartFocus válido: %v", err)
	}

	// 3) segundo start com foco ativo → PK/CHECK(id=1) → ErrFocusActive (409).
	if err := s.StartFocus(id); !errors.Is(err, ErrFocusActive) {
		t.Fatalf("foco duplo: esperava ErrFocusActive, veio %v", err)
	}

	// 4) stop grava a sessão (mínimo 1 min) e limpa o estado.
	dur, hID, err := s.StopFocus()
	if err != nil {
		t.Fatalf("StopFocus: %v", err)
	}
	if dur < 1 || hID != id {
		t.Fatalf("StopFocus devolveu dur=%d habit=%d (esperava dur>=1 habit=%d)", dur, hID, id)
	}

	// 5) stop sem foco ativo → ErrNoActiveFocus (409).
	if _, _, err := s.StopFocus(); !errors.Is(err, ErrNoActiveFocus) {
		t.Fatalf("stop ocioso: esperava ErrNoActiveFocus, veio %v", err)
	}
}

// TestConcurrencyNoLock martela o banco de várias goroutines para provar que
// SetMaxOpenConns(1) + busy_timeout eliminam o "database is locked".
func TestConcurrencyNoLock(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateHabit("Codar", "", "daily")
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
			_ = s.StartFocus(id)
			if _, err := s.GetActiveFocus(); err != nil {
				errCh <- err
			}
			if _, err := s.GetHabits(); err != nil {
				errCh <- err
			}
			if _, _, err := s.StopFocus(); err != nil && !errors.Is(err, ErrNoActiveFocus) {
				errCh <- err
			}
			if _, err := s.LogFocusSession(id, 1); err != nil {
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
	path := filepath.Join(b.TempDir(), "bench.db")
	s, err := NewStore(path)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	id, _ := s.CreateHabit("Codar", "", "daily")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.LogFocusSession(id, 1); err != nil {
			b.Fatalf("LogFocusSession: %v", err)
		}
	}
}
