package main

// report.go — Fase 6: o valor visível. Agrega heartbeats em "minutos ativos"
// por projeto × linguagem e renderiza um relatório de texto puro alinhado,
// com barras proporcionais — bonito num terminal, num curl e num GIF.
//
// O estimador é honesto por construção: um minuto conta como ativo se recebeu
// ao menos um heartbeat. Como os clientes descarregam a cada ≤15s, todo minuto
// realmente codando tem heartbeat; minutos de leitura/ócio não têm. Nenhuma
// extrapolação — se o número impressiona, é porque o trabalho existiu.

import (
	"fmt"
	"strings"
)

// ReportRow é uma linha agregada do relatório de atividade.
type ReportRow struct {
	Project  string
	Language string
	Minutes  int64
}

// barra desenha uma barra proporcional de até `width` células, com oitavos
// de bloco na ponta para não serrilhar valores próximos. Nunca vazia se v>0.
func barra(v, max int64, width int) string {
	if v <= 0 || max <= 0 {
		return ""
	}
	eighths := v * int64(width) * 8 / max
	if eighths == 0 {
		eighths = 1
	}
	full := eighths / 8
	rest := eighths % 8
	b := strings.Repeat("█", int(full))
	if rest > 0 {
		b += string([]rune("▏▎▍▌▋▊▉")[rest-1])
	}
	return b
}

// renderReport monta o relatório completo em texto puro.
func renderReport(days int, rows []ReportRow, focusMin, focusSessions int64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "focusd · last %d days\n\n", days)

	if len(rows) == 0 && focusMin == 0 {
		sb.WriteString("no activity recorded yet — go write some code, it counts itself.\n")
		return sb.String()
	}

	pw, lw := len("PROJECT"), len("LANGUAGE")
	var max, total int64
	for _, r := range rows {
		if len(r.Project) > pw {
			pw = len(r.Project)
		}
		if len(r.Language) > lw {
			lw = len(r.Language)
		}
		if r.Minutes > max {
			max = r.Minutes
		}
		total += r.Minutes
	}

	fmt.Fprintf(&sb, "%-*s  %-*s  %6s\n", pw, "PROJECT", lw, "LANGUAGE", "ACTIVE")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%-*s  %-*s  %5dm  %s\n", pw, r.Project, lw, r.Language, r.Minutes, barra(r.Minutes, max, 16))
	}

	fmt.Fprintf(&sb, "\ncoding: %dm", total)
	if focusSessions > 0 {
		fmt.Fprintf(&sb, " · deliberate focus: %dm across %d session(s)", focusMin, focusSessions)
	}
	sb.WriteString("\n")
	return sb.String()
}
