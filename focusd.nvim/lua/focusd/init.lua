-- focusd.nvim — telemetria passiva de fricção zero para o daemon focusd.
--
-- Arquitetura (um módulo por responsabilidade, I/O confinado ao transport):
--   collector.lua  captura + coalescing O(1) em memória
--   transport.lua  HTTP/1.1 cru no Unix socket via vim.uv (timeout 250ms,
--                  latest-wins, backoff, autostart)
--   status.lua     cache assíncrono do /status para a statusline
--   health.lua     :checkhealth focusd

local config = require("focusd.config")

local M = {}

local function notify(status, body, ok_level)
	if status and status < 400 then
		vim.notify(body, ok_level or vim.log.levels.INFO)
	else
		vim.notify("focusd: " .. tostring(body), vim.log.levels.WARN)
	end
end

function M.setup(opts)
	config.setup(opts)
	require("focusd.collector").start()
	require("focusd.status").start()

	vim.api.nvim_create_user_command("FocusdFlush", function()
		require("focusd.collector").flush()
	end, { desc = "Envia os heartbeats pendentes agora" })

	vim.api.nvim_create_user_command("FocusdStatus", function()
		require("focusd.transport").get("/status", nil, notify)
	end, { desc = "Mostra o status de foco do daemon" })

	vim.api.nvim_create_user_command("FocusdFocusStart", function(a)
		require("focusd.transport").post_form("/focus/start", "habit_id=" .. a.args, nil, notify)
	end, { nargs = 1, desc = "Inicia sessão de foco para um hábito (id)" })

	vim.api.nvim_create_user_command("FocusdFocusStop", function()
		require("focusd.transport").post_form("/focus/stop", "", nil, notify)
	end, { desc = "Encerra a sessão de foco ativa" })
end

-- Componente pronto para statusline (leitura pura de memória):
--   lualine: sections = { lualine_x = { require("focusd").status } }
function M.status()
	return require("focusd.status").get()
end

return M
