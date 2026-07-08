-- health.lua — :checkhealth focusd. Diagnóstico honesto em quatro eixos:
-- versão do Neovim, socket no filesystem, binário no PATH e um round-trip
-- REAL ao daemon respeitando o mesmo contrato de 250ms do resto do plugin.

local uv = vim.uv or vim.loop
local config = require("focusd.config")
local transport = require("focusd.transport")
local collector = require("focusd.collector")

local M = {}

function M.check()
	local h = vim.health
	h.start("focusd")

	if vim.fn.has("nvim-0.10") ~= 1 then
		h.error("Neovim >= 0.10 é necessário (vim.uv, vim.fs.root)")
		return
	end
	h.ok("Neovim " .. tostring(vim.version()))

	local sock = config.socket_path()
	local st = uv.fs_stat(sock)
	if st and st.type == "socket" then
		h.ok("socket: " .. sock)
	else
		h.warn("socket ainda não existe: " .. sock, {
			"com autostart=true o daemon sobe sozinho na primeira atividade",
			"ou rode `focusd` manualmente",
		})
	end

	if vim.fn.executable(config.options.bin) == 1 then
		h.ok(("binário '%s' encontrado no PATH"):format(config.options.bin))
	elseif config.options.autostart then
		h.warn(("binário '%s' não está no PATH — autostart não funcionará"):format(config.options.bin))
	end

	-- Round-trip real. vim.wait aqui é aceitável: checkhealth é uma ação
	-- explícita do usuário, e mesmo assim honramos o teto de timeout.
	local done, status, body = false, nil, nil
	transport.get("/status", nil, function(s, b)
		done, status, body = true, s, b
	end)
	vim.wait(config.options.timeout_ms + 100, function()
		return done
	end, 10)

	if status == 200 then
		h.ok("daemon respondeu: " .. body)
	else
		h.error("daemon não respondeu: " .. tostring(body))
	end

	local stats = transport.stats()
	h.ok(("transporte: %d falha(s) consecutivas, backoff restante %dms, %d heartbeat(s) pendentes")
		:format(stats.failures, stats.down_for_ms, collector.pending_count()))
end

return M
