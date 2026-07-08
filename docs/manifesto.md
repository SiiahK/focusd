# 

```text
  ███████╗ ██████╗  ██████╗██╗   ██╗███████╗██████╗
  ██╔════╝██╔═══██╗██╔════╝██║   ██║██╔════╝██╔══██╗
  █████╗  ██║   ██║██║     ██║   ██║███████╗██║  ██║
  ██╔══╝  ██║   ██║██║     ██║   ██║╚════██║██║  ██║
  ██║     ╚██████╔╝╚██████╗╚██████╔╝███████║██████╔╝
  ╚═╝      ╚═════╝  ╚═════╝ ╚═════╝ ╚══════╝╚═════╝

        o daemon de foco que mora no seu terminal
```

> **focusd** é um rastreador de hábitos e sessões de foco, local-first, escrito
> em Go + SQLite, com integração nativa para **tmux** e **Neovim** e um painel
> web em **HTMX** vestido de **Catppuccin Mocha**. Sem conta. Sem nuvem. Sem
> telemetria. Os seus dados nunca saem da sua máquina.

---

## O Manifesto

Existe uma guerra em curso pela sua atenção — e você está perdendo por padrão.

Cada aba aberta, cada notificação, cada "só vou dar uma olhadinha" é uma
pequena derrota que não aparece em lugar nenhum. O problema não é falta de
disciplina; é falta de **placar**. O que não é medido não é defendido.

Os apps de produtividade tradicionais tentam resolver isso te arrancando do
seu ambiente: abra o navegador, faça login, aceite os cookies, ignore o upsell.
Ou seja — para registrar que você estava focado, eles primeiro te desfocam.
É o incêndio vendendo extintor.

O **focusd** parte do princípio oposto: **a ferramenta vai até onde o trabalho
acontece, nunca o contrário.** Se a sua vida roda no terminal, o seu placar de
foco tem que morar no terminal:

- **Onipresença sem atrito.** O cronômetro pulsa na barra do tmux e na lualine
  do Neovim. Você nunca troca de contexto para consultá-lo — ele simplesmente
  está lá, como o relógio na parede.
- **Silêncio quando ocioso.** Sem foco ativo (ou com o daemon parado), a barra
  fica **vazia**. Nada de ícone cinza, nada de "0m", nada de ruído cognitivo.
  O focusd só fala quando tem algo a dizer.
- **Nunca atrapalhar.** O componente da status-line usa `curl --max-time 0.4`;
  o plugin de Neovim faz polling assíncrono via libuv. Se o servidor cair, o
  seu editor e o seu tmux não percebem. O rastreador jamais pode custar o foco
  que ele existe para proteger.
- **Seus dados, seu disco.** Tudo vive num único arquivo SQLite em
  `~/.focusd/focus.db`. Backup é `cp`. Exportação é `sqlite3`. Auditoria é
  abrir o arquivo. Para sempre.

Foco não se gerencia num SaaS. Se cultiva onde você vive.

---

## Requisitos

| Dependência | Por quê | Verificação |
|---|---|---|
| **Go ≥ 1.21** | compilar o daemon | `go version` |
| **Compilador C** (gcc/clang) | o driver SQLite usa CGO | `cc --version` |
| **curl** | integrações tmux/nvim falam HTTP | `curl --version` |
| **tmux ≥ 2.9** *(opcional)* | cores hex na status-line | `tmux -V` |
| **Neovim ≥ 0.9** *(opcional)* | plugin `focus_tracker.lua` | `nvim --version` |

No macOS, `xcode-select --install` cobre o compilador C. Em Debian/Ubuntu,
`sudo apt install build-essential`.

---

## Instalação

### 1. Compile e instale

Na raiz do projeto (esta pasta do .zip):

```sh
make install
```

Isso compila um binário otimizado e distribui cada peça no seu lugar:

```text
~/.focusd/focus-tracker      # o daemon
~/.focusd/focusd.sh          # controle start/stop/status/logs
~/.focusd/focus_status.sh    # componente da status-line do tmux
~/bin/focusd                 # symlink de conveniência para o controle
~/bin/tmux_focus.sh          # log rápido de sessão via keybind
~/.config/nvim/lua/focus_tracker.lua   # plugin do Neovim
```

Se você já usava uma versão anterior com um `focus.db` na pasta, o `install`
**migra o seu histórico automaticamente** para `~/.focusd/` — e nunca
sobrescreve um banco que já exista lá.

> Quer instalar em outro lugar? Todos os caminhos aceitam override:
> `FOCUSD_HOME=/opt/focusd BIN_DIR=/usr/local/bin make install`

### 2. Suba o daemon

```sh
make start        # ou, de qualquer lugar: focusd start
```

O daemon roda em background, silencioso, escutando **apenas em
`127.0.0.1:8080`** (nada é exposto na sua rede). Comandos do dia a dia:

```sh
focusd status     # está rodando? qual o foco ativo?
focusd stop       # encerra
focusd restart    # reinicia
focusd logs       # acompanha o log (Ctrl-C para sair)
```

Porta ocupada? Suba com `FOCUSD_ADDR=127.0.0.1:9090` e ajuste `FOCUSD_URL`
nas integrações.

### 3. Injete no `~/.tmux.conf`

Adicione ao final do seu `~/.tmux.conf`:

```tmux
# ── focusd ──────────────────────────────────────────────────────────
# Cronômetro de foco na status-line (o dot ● pulsa a cada segundo)
set -g status-interval 1
set -g status-right '#(~/.focusd/focus_status.sh) %H:%M'

# prefix + F  →  registra uma sessão de foco de 25min no hábito 1
bind-key F run-shell "~/bin/tmux_focus.sh 1 25"
```

Recarregue com `tmux source-file ~/.tmux.conf`. Enquanto houver um foco
ativo, a barra mostra `● 🎯 15m` em verde Catppuccin; quando não houver,
ela fica limpa — exatamente como manda o manifesto.

### 4. Injete no `init.lua` do Neovim

O `make install` já colocou o plugin no `runtimepath`. Basta ativá-lo:

```lua
-- ── focusd ──────────────────────────────────────────────────────────
require("focus_tracker").setup()
```

Você ganha dois comandos:

```vim
:FocusStart 1     " inicia o cronômetro no hábito de id 1
:FocusStop        " encerra e grava a duração no banco
```

E, se usa **lualine**, um componente pronto que faz polling assíncrono
(nunca bloqueia o editor):

```lua
require("lualine").setup({
  sections = {
    lualine_x = { require("focus_tracker").status },
  },
})
```

O cronômetro é do **servidor**, não do editor: comece um foco no Neovim,
feche tudo, e o tmux continua contando. Uma fonte da verdade.

---

## O Painel — `http://localhost:8080`

Com o daemon no ar, abra [http://localhost:8080](http://localhost:8080).

Você encontra o painel do focusd em **Catppuccin Mocha** — o fundo
`base #1e1e2e`, o verde `#a6e3a1` do cronômetro (o mesmo da sua barra do
tmux), a paleta inteira consistente com o seu terminal. Abrir o painel não
deve parecer "sair do terminal"; deve parecer que o terminal ganhou uma
segunda tela.

Ali você pode:

- **Criar e acompanhar hábitos**, com o histórico de sessões de cada um;
- **Ver o foco ativo em tempo real** — o painel usa HTMX para atualizar por
  fragmentos, sem recarregar a página e sem nenhum framework JS pesado
  (o próprio htmx vem embutido no binário; o painel funciona 100% offline);
- **Registrar sessões manualmente**, para aquele foco que aconteceu longe
  do teclado.

### API — para os seus próprios scripts

Tudo que o painel e as integrações fazem passa por HTTP local. Componha à
vontade:

| Rota | Método | Função |
|---|---|---|
| `/status` | GET | texto puro ultra-leve (ideal p/ status-lines) |
| `/focus/start` | POST | inicia uma sessão ativa |
| `/focus/stop` | POST | encerra a sessão e grava a duração |
| `/focus` | POST | registra uma sessão já concluída |
| `/habits` | GET / POST | lista / cria hábitos |

```sh
curl -X POST -d 'habit_id=1' http://localhost:8080/focus/start
curl http://localhost:8080/status        # → 🎯 Foco: 15m
curl -X POST http://localhost:8080/focus/stop
```

---

## Anatomia

```text
                        ┌───────────────────────────┐
   tmux status-line ──▶ │                           │
   Neovim (lualine) ──▶ │   focusd  ·  127.0.0.1    │ ──▶ ~/.focusd/focus.db
   painel (HTMX)    ──▶ │   Go · Echo · embed.FS    │      (SQLite, WAL)
   seus scripts     ──▶ │                           │
                        └───────────────────────────┘
```

Um daemon. Um banco. Um arquivo de verdade. Todo o resto — barra do tmux,
plugin do Neovim, painel web — são apenas janelas para o mesmo estado.

---

## Solução de problemas

- **A barra do tmux não mostra nada.** Provavelmente está tudo certo: sem
  foco ativo, a barra fica limpa por design. Rode
  `curl http://localhost:8080/status` — se responder vazio, inicie um foco
  e observe a barra acordar.
- **`make build` falha reclamando de CGO.** Falta o compilador C — veja a
  tabela de requisitos acima.
- **Mudei a porta e as integrações pararam.** Exporte
  `FOCUSD_URL=http://localhost:9090` no seu shell (o `focus_status.sh` lê
  essa variável) e ajuste o `BASE_URL` no topo do `focus_tracker.lua`.
- **Quero começar do zero.** `focusd stop && rm ~/.focusd/focus.db*` — mas
  faça um `cp` antes. Histórico de foco é patrimônio.

---

## Filosofia de manutenção

O focusd é deliberadamente pequeno: um binário, um banco, três scripts e um
arquivo Lua. Cada linha existe para ser lida. Se algo quebrar, você tem tudo
o que precisa para consertar — e é assim que uma ferramenta de terminal deve
envelhecer.

**Agora feche esta aba e vá focar.** `:FocusStart 1`

---

*focusd — porque o que não é medido não é defendido.*
