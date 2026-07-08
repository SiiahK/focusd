package main

// cli.go — modo cliente do binário focusd (Fase 5, "shipping honesto").
//
// O mesmo executável é daemon (sem argumentos) e CLI (com subcomando):
//
//	focusd                    sobe o daemon (singleton via flock)
//	focusd init --hook        instala o hook post-commit no repo atual
//	focusd hook post-commit   chamado PELO hook: registra o commit e mostra o streak
//
// Filosofia do caminho de commit: telemetria passiva NUNCA atrapalha. O hook
// tem o mesmo contrato de 250ms dos outros clientes, autostarta o daemon se
// preciso (seguro: flock) e, se nada funcionar, sai em silêncio com exit 0 —
// um commit jamais falha ou trava por causa do focusd.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"context"
)

const hookMarker = "# focusd post-commit hook"

// version é injetada nos builds de release via ldflags (goreleaser:
// -X main.version={{.Version}}). "dev" identifica builds locais.
var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `focusd — local-first focus engine

uso:
  focusd                     sobe o daemon (idempotente: segunda instância sai em silêncio)
  focusd init --hook         instala o hook post-commit no repositório git atual
  focusd hook post-commit    usado pelo hook instalado; registra o commit no focusd
  focusd report [--days N]   minutos ativos por projeto × linguagem (default: 7 dias)
  focusd version             versão do binário

config por ambiente: FOCUSD_DB, FOCUSD_SOCK, FOCUSD_ADDR (UI web opcional)
`)
}

// runCLI trata o subcomando e encerra o processo (o daemon é só o modo sem args).
func runCLI(args []string) {
	switch args[0] {
	case "init":
		cmdInit(args[1:])
	case "hook":
		cmdHook(args[1:])
	case "report":
		cmdReport(args[1:])
	case "version", "--version", "-v":
		fmt.Println("focusd " + version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "focusd: subcomando desconhecido %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func cmdInit(args []string) {
	hook := false
	for _, a := range args {
		if a == "--hook" {
			hook = true
		} else {
			fmt.Fprintf(os.Stderr, "focusd init: flag desconhecida %q\n\n", a)
			usage()
			os.Exit(2)
		}
	}
	if !hook {
		usage()
		os.Exit(2)
	}
	if err := installHook(); err != nil {
		fmt.Fprintf(os.Stderr, "focusd init: %v\n", err)
		os.Exit(1)
	}
}

func cmdHook(args []string) {
	if len(args) != 1 || args[0] != "post-commit" {
		usage()
		os.Exit(2)
	}
	runPostCommit()
}

// cmdReport busca /report no daemon e imprime. Mesmo espírito do hook:
// autostart se o daemon estiver frio, mas aqui falha é VISÍVEL (o usuário
// pediu o relatório de forma explícita; silêncio seria gaslighting).
func cmdReport(args []string) {
	days := 7
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--days" && i+1 < len(args):
			i++
			a = "--days=" + args[i]
			fallthrough
		case strings.HasPrefix(a, "--days="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--days="))
			if err != nil || n < 1 || n > 365 {
				fmt.Fprintln(os.Stderr, "focusd report: --days precisa ser um inteiro entre 1 e 365")
				os.Exit(2)
			}
			days = n
		default:
			fmt.Fprintf(os.Stderr, "focusd report: flag desconhecida %q\n\n", a)
			usage()
			os.Exit(2)
		}
	}

	client := daemonClient()
	url := fmt.Sprintf("http://focusd/report?days=%d", days)
	body, ok := getText(client, url)
	if !ok {
		spawnDaemon()
		for i := 0; i < 4 && !ok; i++ {
			time.Sleep(150 * time.Millisecond)
			body, ok = getText(client, url)
		}
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "focusd report: daemon não respondeu (socket: "+socketPath()+")")
		os.Exit(1)
	}
	fmt.Print(body)
}

// getText faz um GET e devolve o corpo como texto (ok=false em qualquer falha).
func getText(client *http.Client, url string) (string, bool) {
	resp, err := client.Get(url)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", false
	}
	return string(body), true
}

// findGitDir sobe a partir do cwd até achar .git. Devolve a raiz do worktree
// e o diretório git real (resolvendo o arquivo "gitdir:" de worktrees/submódulos).
func findGitDir() (root, gitDir string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for {
		g := filepath.Join(dir, ".git")
		if fi, statErr := os.Stat(g); statErr == nil {
			if fi.IsDir() {
				return dir, g, nil
			}
			b, readErr := os.ReadFile(g)
			if readErr != nil {
				return "", "", readErr
			}
			p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			return dir, p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", errors.New("não é um repositório git (nenhum .git encontrado subindo a árvore)")
		}
		dir = parent
	}
}

// installHook grava o post-commit em <gitdir>/hooks. Um hook existente que não
// é nosso NÃO é sobrescrito — hooks alheios são código do usuário; oferecemos
// a linha para integração manual. Um hook nosso (marcador presente) é atualizado.
func installHook() error {
	_, gitDir, err := findGitDir()
	if err != nil {
		return err
	}
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(hooksDir, "post-commit")

	// PATH primeiro (sobrevive a upgrades via go install); caminho absoluto
	// gravado na instalação como fallback para quem não tem ~/go/bin no PATH.
	exe, err := os.Executable()
	if err != nil {
		exe = "focusd"
	}
	script := "#!/bin/sh\n" +
		hookMarker + " — instalado por `focusd init --hook`.\n" +
		"# Telemetria passiva: nunca bloqueia nem falha um commit.\n" +
		"FOCUSD_BIN=" + strconv.Quote(exe) + "\n" +
		"command -v focusd >/dev/null 2>&1 && FOCUSD_BIN=focusd\n" +
		"\"$FOCUSD_BIN\" hook post-commit 2>/dev/null || true\n"

	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !strings.Contains(string(existing), hookMarker) {
			return fmt.Errorf(
				"já existe um post-commit seu em %s — não vou sobrescrever.\n"+
					"Adicione esta linha ao seu hook:\n\n    focusd hook post-commit 2>/dev/null || true",
				path)
		}
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	fmt.Printf("✓ hook post-commit instalado em %s\n", path)
	fmt.Println("  cada commit agora alimenta seu hábito e streak — zero check-in manual.")
	return nil
}

// daemonClient fala HTTP com o daemon através do Unix socket, sob o mesmo
// contrato de 250ms de todos os clientes focusd.
func daemonClient() *http.Client {
	sock := socketPath()
	return &http.Client{
		Timeout: 250 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// spawnDaemon relança o próprio executável em modo daemon, desacoplado da
// sessão (Setsid). Corrida de spawns é resolvida pelo flock do daemon.
func spawnDaemon() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err == nil {
		_ = cmd.Process.Release()
	}
}

func postHeartbeat(client *http.Client, payload []byte) bool {
	resp, err := client.Post("http://focusd/heartbeat", "application/json", bytes.NewReader(payload))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func fetchStreak(client *http.Client) int {
	resp, err := client.Get("http://focusd/streak")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32))
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(body)))
	return n
}

// runPostCommit é o corpo do hook: um heartbeat com source=git e o streak
// atualizado na resposta. Qualquer falha termina em silêncio, exit 0.
func runPostCommit() {
	project := ""
	if root, _, err := findGitDir(); err == nil {
		project = filepath.Base(root)
	}
	payload, err := json.Marshal(heartbeatPayload{
		Source: "git",
		Heartbeats: []Heartbeat{
			{Project: project, Language: "git", Events: 1, At: time.Now().Unix()},
		},
	})
	if err != nil {
		return
	}

	client := daemonClient()
	ok := postHeartbeat(client, payload)
	if !ok {
		// Daemon fora: autostart + retries curtos. Orçamento total do hook
		// fica em ~1s no pior caso — perceptível, mas só na primeira vez.
		spawnDaemon()
		for i := 0; i < 4 && !ok; i++ {
			time.Sleep(150 * time.Millisecond)
			ok = postHeartbeat(client, payload)
		}
	}
	if !ok {
		return
	}

	if streak := fetchStreak(client); streak > 0 {
		unit := "dias"
		if streak == 1 {
			unit = "dia"
		}
		fmt.Printf("🚀 focusd · commit registrado · streak: %d %s 🔥\n", streak, unit)
	} else {
		fmt.Println("🚀 focusd · commit registrado")
	}
}
