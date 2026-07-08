-- transport.lua — o ÚNICO módulo que toca I/O. Fala HTTP/1.1 cru direto no
-- Unix socket via pipe do libuv (um pipe libuv É um unix domain socket):
-- zero forks de curl por evento, zero threads, zero bloqueio do main loop.
--
-- Contratos deste módulo:
--   • timeout duro de config.timeout_ms (250ms): estourou, fecha o pipe e
--     reporta erro — o Neovim nunca fica refém do daemon;
--   • "latest wins" por chave: nova request com a mesma key cancela a
--     anterior em voo (um poll de status atrasado é lixo, não backlog);
--   • backoff exponencial quando o daemon está fora — falha instantânea sem
--     nem tentar conectar, e (opcional) autostart do daemon, que é seguro
--     porque o flock do focusd faz spawns concorrentes saírem silenciosos;
--   • callbacks sempre re-entregues via vim.schedule — quem consome pode
--     tocar a API do editor sem pensar em fast-event context.
--
-- Parsing: mandamos "Connection: close", então a resposta completa é
-- "tudo até EOF" — sem state machine de keep-alive. Os endpoints que este
-- plugin consome respondem texto curto com Content-Length (nunca chunked).

local uv = vim.uv or vim.loop
local config = require("focusd.config")

local M = {}

local inflight = {} -- key → função finish() da request em voo (latest wins)
local failures = 0
local down_until = 0 -- uv.now() ms: fail-fast até este instante
local last_spawn = 0

local function backoff_on_failure()
	failures = failures + 1
	local wait = math.min(config.options.max_backoff_ms, 1000 * 2 ^ (failures - 1))
	down_until = uv.now() + wait
end

-- try_autostart sobe o daemon em background, no máximo uma vez a cada 10s.
-- Agendado para o main loop: vim.fn.executable não pode rodar em fast context.
local function try_autostart()
	if not config.options.autostart or uv.now() - last_spawn < 10000 then
		return
	end
	last_spawn = uv.now()
	vim.schedule(function()
		local bin = config.options.bin
		if vim.fn.executable(bin) ~= 1 then
			return
		end
		local ok, proc = pcall(uv.spawn, bin, { detached = true, hide = true }, function() end)
		if ok and proc then
			proc:unref()
		end
	end)
end

--- Dispara uma request assíncrona. cb(status, body) em sucesso HTTP (qualquer
--- status), cb(nil, motivo) em falha de transporte. cb SEMPRE roda no main loop.
---@param opts {method:string, path:string, body:string?, content_type:string?, key:string?, cb:fun(status:integer?, body:string)}
function M.request(opts)
	local o = config.options
	local done = false
	local pipe, timer

	local function finish(status, body)
		if done then
			return
		end
		done = true
		if timer and not timer:is_closing() then
			timer:stop()
			timer:close()
		end
		if pipe and not pipe:is_closing() then
			pipe:close()
		end
		if opts.key and inflight[opts.key] == finish then
			inflight[opts.key] = nil
		end
		if status then
			failures, down_until = 0, 0
		elseif body ~= "superseded" and body ~= "backoff" then
			-- só falhas REAIS de tentativa contam para o backoff; fail-fast
			-- durante o próprio backoff não pode compor a espera.
			backoff_on_failure()
		end
		vim.schedule(function()
			opts.cb(status, body)
		end)
	end

	-- latest wins: derruba a request anterior da mesma chave antes de partir.
	if opts.key then
		local prev = inflight[opts.key]
		if prev then
			prev(nil, "superseded")
		end
		inflight[opts.key] = finish
	end

	if uv.now() < down_until then
		finish(nil, "backoff")
		return
	end

	pipe = uv.new_pipe(false)
	timer = uv.new_timer()
	timer:start(o.timeout_ms, 0, function()
		finish(nil, "timeout")
	end)

	pipe:connect(config.socket_path(), function(cerr)
		if cerr then
			try_autostart()
			finish(nil, "connect: " .. cerr)
			return
		end
		local body = opts.body or ""
		pipe:write(table.concat({
			string.format("%s %s HTTP/1.1", opts.method, opts.path),
			"Host: focusd",
			"Connection: close",
			"Content-Type: " .. (opts.content_type or "application/json"),
			"Content-Length: " .. #body,
			"",
			body,
		}, "\r\n"))

		local chunks = {}
		pipe:read_start(function(rerr, chunk)
			if rerr then
				finish(nil, "read: " .. rerr)
				return
			end
			if chunk then
				chunks[#chunks + 1] = chunk
				return
			end
			-- EOF (Connection: close) = resposta completa.
			local resp = table.concat(chunks)
			local status = tonumber(resp:match("^HTTP/1%.%d (%d%d%d)"))
			local _, hdr_end = resp:find("\r\n\r\n", 1, true)
			if not status or not hdr_end then
				finish(nil, "resposta malformada")
				return
			end
			finish(status, resp:sub(hdr_end + 1))
		end)
	end)
end

function M.get(path, key, cb)
	M.request({ method = "GET", path = path, key = key, cb = cb })
end

function M.post_json(path, payload, key, cb)
	M.request({
		method = "POST",
		path = path,
		body = vim.json.encode(payload),
		content_type = "application/json",
		key = key,
		cb = cb,
	})
end

function M.post_form(path, form, key, cb)
	M.request({
		method = "POST",
		path = path,
		body = form,
		content_type = "application/x-www-form-urlencoded",
		key = key,
		cb = cb,
	})
end

-- stats expõe o estado interno para o :checkhealth.
function M.stats()
	return { failures = failures, down_for_ms = math.max(0, down_until - uv.now()) }
end

return M
