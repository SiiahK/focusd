# focusd.nvim

Telemetria passiva de fricção zero para o [focusd](../README.md): você coda,
os hábitos se atualizam sozinhos. 100% local, 100% assíncrono — o plugin fala
HTTP cru direto no Unix socket do daemon via `vim.uv`, sem `curl`, sem
Electron, sem nuvem.

Requer Neovim >= 0.10 e o binário `focusd` (o plugin sobe o daemon sozinho
se ele não estiver rodando; o flock do focusd torna isso idempotente).

## Instalação (lazy.nvim)

```lua
{
  dir = "~/Downloads/focusd/focusd.nvim", -- ou a URL do repo publicado
  event = "VeryLazy",
  opts = {
    -- defaults; tudo opcional:
    -- sock = nil,               -- auto-descoberta (FOCUSD_SOCK → XDG → cache)
    -- bin = "focusd",
    -- autostart = true,
    -- timeout_ms = 250,          -- o editor NUNCA espera mais que isto
    -- flush_interval_ms = 15000,
    -- status_interval_ms = 5000,
  },
}
```

## Statusline (lualine)

```lua
sections = { lualine_x = { require("focusd").status } }
```

`require("focusd").status()` é uma leitura pura de memória — pode ser chamada
a cada redraw sem custo. Quem a atualiza é um poll assíncrono de 5s, pausado
quando o Neovim perde o foco do SO.

## Comandos

- `:FocusdStatus` — status de foco atual
- `:FocusdFocusStart {habit_id}` / `:FocusdFocusStop` — controla a sessão
- `:FocusdFlush` — envia os heartbeats pendentes agora
- `:checkhealth focusd` — diagnóstico (socket, binário, round-trip real)

## Como funciona

Autocmds (`TextChanged`, `BufWritePost`, `BufEnter`, …) apenas incrementam
contadores numa tabela Lua (O(1), zero I/O no caminho de digitação). Um timer
descarrega o acumulado a cada 15s — ou imediatamente no save — num único
`POST /heartbeat` em lote. Toda request tem timeout duro de 250ms, requests
da mesma classe se cancelam ("latest wins") e, com o daemon fora, o plugin
entra em backoff exponencial e degrada em silêncio: o Neovim nunca trava.
