package main

// daemon.go — plumbing do daemon focusd (Fase 2, "a armadura"):
//   • diretórios XDG com fallback pra macOS (que NÃO seta XDG_RUNTIME_DIR);
//   • garantia de instância única via flock advisory (singleton REAL, no
//     binário — independe de wrapper shell);
//   • listener Unix domain socket (0600) no lugar do TCP;
//   • logger rotativo (o daemon é invisível: nunca cospe no terminal).

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// runtimeDir devolve o diretório volátil por-sessão do focusd (socket + lock).
// Preferimos XDG_RUNTIME_DIR (tmpfs, limpo no logout); macOS não o define,
// então caímos no UserCacheDir (~/Library/Caches no mac, ~/.cache no Linux).
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "focusd")
	}
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "focusd")
	}
	return filepath.Join(os.TempDir(), "focusd")
}

// stateDir devolve onde vivem os logs (XDG_STATE_HOME ou ~/.local/state).
func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "focusd")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "focusd")
	}
	return filepath.Join(os.TempDir(), "focusd")
}

// socketPath e lockPath derivam de runtimeDir. FOCUSD_SOCK permite override
// (útil em testes e para os clientes tmux/nvim apontarem ao mesmo lugar).
func socketPath() string {
	if s := os.Getenv("FOCUSD_SOCK"); s != "" {
		return s
	}
	return filepath.Join(runtimeDir(), "focusd.sock")
}

func lockPath() string {
	return filepath.Join(runtimeDir(), "focusd.lock")
}

// acquireSingleton pega um flock exclusivo não-bloqueante. Se outra instância
// já roda, retorna erro na hora (LOCK_NB) — o chamador sai limpo e silencioso.
// O *os.File DEVE permanecer aberto por toda a vida do processo (fechar libera
// o lock). Ordem importa: trave ANTES de tocar no socket (§listenUnix).
func acquireSingleton(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("outra instância do focusd já roda: %w", err)
	}
	return f, nil
}

// listenUnix abre o socket 0600. O os.Remove do socket "stale" só é seguro
// porque já seguramos o flock: garante que não há daemon vivo dono do socket,
// então não estamos apagando o socket de ninguém.
func listenUnix(path string) (net.Listener, error) {
	// sun_path tem limite RÍGIDO (~104 bytes no macOS, 108 no Linux). Um
	// caminho longo falha com um críptico "bind: invalid argument"; trocamos
	// por um erro acionável apontando o override FOCUSD_SOCK.
	if len(path) > 100 {
		return nil, fmt.Errorf(
			"caminho do socket longo demais (%d bytes, limite ~104): %s — "+
				"defina FOCUSD_SOCK para um caminho curto", len(path), path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path) // limpa socket órfão de um crash anterior
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// newRotatingLogger direciona TODA saída de log para um arquivo rotativo em
// stateDir — nunca para o terminal. 5MB por arquivo, 3 backups, 28 dias.
// Um daemon que roda pra sempre não pode ter log crescendo sem limite nem
// vazar ruído no shell do usuário.
func newRotatingLogger() *lumberjack.Logger {
	_ = os.MkdirAll(stateDir(), 0o700)
	return &lumberjack.Logger{
		Filename:   filepath.Join(stateDir(), "focusd.log"),
		MaxSize:    5, // MB
		MaxBackups: 3,
		MaxAge:     28, // dias
		Compress:   true,
	}
}

// silenceStdlog aponta o log padrão para o writer rotativo. Chamado cedo, no
// começo do main, para que qualquer log.Printf/Fatalf já caia no arquivo.
func silenceStdlog(w *lumberjack.Logger) {
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("focusd ")
}
