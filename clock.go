package main

// clock.go — Fase 3: correção do time-jump monotônico.
//
// O started_at da sessão é wall clock (unix epoch persistido no SQLite), então
// "tempo decorrido" = wall de agora − wall do início. Só que o wall clock
// continua andando enquanto o laptop hiberna: sem correção, fechar a tampa à
// noite e abrir de manhã viraria uma "sessão de foco" de 9 horas — telemetria
// passiva que MENTE é pior que nenhuma.
//
// A detecção usa a dualidade dos instantes do Go: time.Now() carrega leitura
// monotônica além do wall clock, e Sub() entre dois instantes que TÊM leitura
// monotônica subtrai o monotônico. No Linux (CLOCK_MONOTONIC) e no macOS
// (CLOCK_UPTIME_RAW) o relógio monotônico PARA durante a suspensão, enquanto
// o wall salta no wake. Logo, entre dois ticks:
//
//	wallGap − monoGap ≈ tempo suspenso (ou salto de NTP/ajuste manual)
//
// O excedente vai para active_focus.suspended_secs — persistido, então a
// correção sobrevive a restart do daemon — e é descontado em MinutesElapsed
// e StopFocus. Saltos de NTP para frente também são descontados de propósito:
// em ambos os casos o wall avançou sem o usuário ter vivido aquele tempo.

import (
	"context"
	"log"
	"time"
)

const (
	// clockTickInterval é a granularidade da detecção: erro máximo de meio
	// minuto na duração, a um custo desprezível para um daemon eterno
	// (2880 ticks/dia; UPDATE no banco só quando há salto E sessão ativa).
	clockTickInterval = 30 * time.Second
	// clockJumpFloor filtra jitter de scheduling (GC, carga da máquina):
	// abaixo disto não é suspensão, é ruído — ignorado para não acumular.
	clockJumpFloor = 2 * time.Second
)

// watchClockJumps roda como goroutine pela vida do daemon, descontando da
// sessão ativa o tempo em que a máquina esteve suspensa. Encerra quando ctx
// (o mesmo do graceful shutdown) é cancelado.
func watchClockJumps(ctx context.Context, store *Store) {
	ticker := time.NewTicker(clockTickInterval)
	defer ticker.Stop()

	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			// Round(0) descarta a leitura monotônica e força a subtração em
			// wall clock; sem ele, Sub usa o monotônico. A diferença entre as
			// duas medições do MESMO intervalo é o tempo que o processo
			// "não viveu" (suspensão coalesce o ticker num único tick tardio).
			monoGap := now.Sub(last)
			wallGap := now.Round(0).Sub(last.Round(0))
			prevTick := last
			last = now

			jump := wallGap - monoGap
			if jump < clockJumpFloor {
				continue
			}

			// prevTick.Unix() limita o desconto a sessões que já existiam
			// antes do salto — uma sessão iniciada logo após o wake não pode
			// herdar a noite de hibernação.
			opCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			n, err := store.AddSuspendedSecs(opCtx, int64(jump/time.Second), prevTick.Unix())
			cancel()
			if err != nil {
				log.Printf("clock-jump: falha ao descontar %s: %v", jump.Round(time.Second), err)
				continue
			}
			if n > 0 {
				log.Printf("clock-jump: %s de suspensão descontados da sessão ativa", jump.Round(time.Second))
			}
		}
	}
}
