-- status.lua — cache do texto de foco para a statusline. A regra de ouro:
-- a função que a lualine chama a cada redraw (M.get) é um acesso a variável
-- local, NADA mais — quem preenche o cache é um poll assíncrono, pausado
-- quando o Neovim perde o foco do SO (sem olhos na statusline, sem tráfego).

local uv = vim.uv or vim.loop
local config = require("focusd.config")
local transport = require("focusd.transport")

local M = {}

local cached = ""
local focused = true
local timer
local augroup

local function poll()
	if not focused then
		return
	end
	transport.get("/status", "status", function(status, body)
		if status == 200 then
			cached = body
		elseif status then
			cached = ""
		end
		-- Falha de transporte: mantém o último valor bom. A statusline
		-- degrada para informação levemente velha em vez de piscar.
	end)
end

-- get é o componente de statusline: leitura pura de memória, sempre seguro.
function M.get()
	return cached
end

function M.start()
	augroup = vim.api.nvim_create_augroup("focusd_status", { clear = true })
	vim.api.nvim_create_autocmd("FocusGained", {
		group = augroup,
		callback = function()
			focused = true
			poll()
		end,
	})
	vim.api.nvim_create_autocmd("FocusLost", {
		group = augroup,
		callback = function()
			focused = false
		end,
	})
	timer = uv.new_timer()
	-- primeiro poll quase imediato (200ms) para a statusline não nascer vazia
	timer:start(200, config.options.status_interval_ms, poll)
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

return M
