-- collector.lua — captura de atividade. A regra do caminho quente: autocmds
-- que disparam a cada tecla/movimento fazem trabalho O(1) e 100% em memória
-- (incrementar um contador numa tabela Lua). TODO o I/O acontece no flush,
-- disparado por timer ou por save — nunca pelo evento em si.
--
-- Semântica de entrega: melhor-esforço, no mínimo "at-least-once". Um flush
-- que falha devolve o lote ao pote (somando contadores, preservando o menor
-- `at`); se o daemon gravou mas a resposta se perdeu, o reenvio duplica o
-- lote — aceitável para telemetria de hábito, inaceitável seria travar o editor.

local uv = vim.uv or vim.loop
local config = require("focusd.config")
local transport = require("focusd.transport")

local M = {}

local pending = {} -- caminho absoluto → heartbeat em construção
local pending_n = 0
local timer
local augroup

-- Teto de arquivos distintos por lote: limita memória E o tamanho do POST
-- (o daemon recusa lotes >1000; nunca chegamos perto).
local MAX_PENDING = 200

-- mark registra atividade no buffer. Barato o bastante para rodar a cada
-- TextChangedI sem culpa: no caso comum (arquivo já no pote) é um único
-- incremento de campo.
function M.mark(buf)
	buf = buf or 0
	if vim.bo[buf].buftype ~= "" then
		return -- terminal, quickfix, prompt, help…: não é código do usuário
	end
	local name = vim.api.nvim_buf_get_name(buf)
	if name == "" then
		return
	end
	local hb = pending[name]
	if hb then
		hb.events = hb.events + 1
		return
	end
	if pending_n >= MAX_PENDING then
		return
	end
	local root = vim.fs.root(buf, ".git")
	pending[name] = {
		project = root and vim.fs.basename(root) or "",
		-- privacidade e legibilidade: caminho RELATIVO à raiz do projeto
		-- quando há uma; absoluto só como fallback.
		file = root and name:sub(#root + 2) or name,
		language = vim.bo[buf].filetype,
		events = 1,
		at = os.time(),
	}
	pending_n = pending_n + 1
end

-- flush envia o pote acumulado num único POST /heartbeat. done_cb (opcional)
-- roda após a tentativa, sucesso ou não — é o gancho do flush de saída.
function M.flush(done_cb)
	if pending_n == 0 then
		if done_cb then
			done_cb()
		end
		return
	end
	local batch = pending
	pending, pending_n = {}, 0

	local list = {}
	for _, hb in pairs(batch) do
		list[#list + 1] = hb
	end

	transport.post_json("/heartbeat", { source = "nvim", heartbeats = list }, "heartbeat", function(status)
		if not status or status >= 500 then
			-- Daemon fora/timeout/superseded: devolve ao pote sem perder
			-- contagem. 4xx NÃO volta (payload que o daemon recusou uma vez
			-- será recusado sempre — reenviar seria loop infinito).
			for name, hb in pairs(batch) do
				local cur = pending[name]
				if cur then
					cur.events = cur.events + hb.events
					cur.at = math.min(cur.at, hb.at)
				elseif pending_n < MAX_PENDING then
					pending[name] = hb
					pending_n = pending_n + 1
				end
			end
		end
		if done_cb then
			done_cb()
		end
	end)
end

function M.start()
	augroup = vim.api.nvim_create_augroup("focusd_collector", { clear = true })

	vim.api.nvim_create_autocmd(
		{ "TextChanged", "TextChangedI", "InsertEnter", "CursorHold", "BufEnter", "FocusGained" },
		{
			group = augroup,
			callback = function(a)
				M.mark(a.buf)
			end,
		}
	)

	-- Save é o momento de maior valor semântico: marca E descarrega na hora
	-- (o "latest wins" do transport absorve rajadas de :wa sem empilhar).
	vim.api.nvim_create_autocmd("BufWritePost", {
		group = augroup,
		callback = function(a)
			M.mark(a.buf)
			M.flush()
		end,
	})

	-- Saída: melhor esforço SÍNCRONO limitado pelo contrato de timeout — o
	-- vim.wait bombeia o main loop até o flush completar ou o prazo estourar.
	-- Se estourar, o último intervalo se perde: aceitável para telemetria;
	-- segurar o :q do usuário além disso não é.
	vim.api.nvim_create_autocmd("VimLeavePre", {
		group = augroup,
		callback = function()
			if pending_n == 0 then
				return
			end
			local flushed = false
			M.flush(function()
				flushed = true
			end)
			vim.wait(config.options.timeout_ms + 50, function()
				return flushed
			end, 10)
		end,
	})

	timer = uv.new_timer()
	timer:start(config.options.flush_interval_ms, config.options.flush_interval_ms, function()
		-- timer roda em fast context; flush mexe em estado + vim.json → main loop
		vim.schedule(M.flush)
	end)
end

function M.stop()
	if timer and not timer:is_closing() then
		timer:stop()
		timer:close()
		timer = nil
	end
	if augroup then
		vim.api.nvim_del_augroup_by_id(augroup)
		augroup = nil
	end
end

-- pending_count expõe o tamanho do pote para o :checkhealth.
function M.pending_count()
	return pending_n
end

return M
