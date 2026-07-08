-- config.lua — configuração compartilhada do focusd.nvim.
--
-- Módulo separado de propósito: todos os outros módulos LEEM daqui e só o
-- init.lua ESCREVE (uma vez, no setup) — sem requires circulares.

local uv = vim.uv or vim.loop

local M = {}

M.defaults = {
	-- Caminho do Unix socket do daemon. nil = auto-descoberta (espelha a
	-- lógica do daemon Go: FOCUSD_SOCK → XDG_RUNTIME_DIR → cache do SO).
	sock = nil,
	-- Binário do daemon para o autostart (procurado no PATH).
	bin = "focusd",
	-- Sobe o daemon sozinho quando o socket não responde. Seguro por design:
	-- o flock do focusd faz spawns concorrentes saírem silenciosos (exit 0).
	autostart = true,
	-- Contrato da Fase 2: o editor NUNCA espera o daemon além disto.
	timeout_ms = 250,
	-- Cadência do flush de heartbeats (saves fazem flush imediato).
	flush_interval_ms = 15000,
	-- Cadência do poll de /status para a statusline.
	status_interval_ms = 5000,
	-- Teto do backoff exponencial quando o daemon está fora.
	max_backoff_ms = 30000,
}

M.options = vim.deepcopy(M.defaults)

function M.setup(opts)
	M.options = vim.tbl_deep_extend("force", {}, M.defaults, opts or {})
end

-- socket_path espelha runtimeDir()/socketPath() do daemon (daemon.go):
-- FOCUSD_SOCK → $XDG_RUNTIME_DIR/focusd/ → cache do usuário (~/Library/Caches
-- no macOS, $XDG_CACHE_HOME ou ~/.cache no Linux).
--
-- IMPORTANTE: chamado de dentro de callbacks de timer do libuv (fast event
-- context), onde vim.env/vim.fn são PROIBIDOS — daí os.getenv, que é Lua puro.
local function getenv(name)
	local v = os.getenv(name)
	if v and v ~= "" then
		return v
	end
	return nil
end

function M.socket_path()
	if M.options.sock then
		return M.options.sock
	end
	local override = getenv("FOCUSD_SOCK")
	if override then
		return override
	end
	local runtime = getenv("XDG_RUNTIME_DIR")
	if runtime then
		return vim.fs.joinpath(runtime, "focusd", "focusd.sock")
	end
	local cache
	if uv.os_uname().sysname == "Darwin" then
		cache = vim.fs.joinpath(getenv("HOME") or "", "Library", "Caches")
	else
		cache = getenv("XDG_CACHE_HOME") or vim.fs.joinpath(getenv("HOME") or "", ".cache")
	end
	return vim.fs.joinpath(cache, "focusd", "focusd.sock")
end

return M
