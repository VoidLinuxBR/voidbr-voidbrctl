# voidbrctl v0.1.3

`systemctl` + `journalctl` + timers do systemd **sobre o runit**, para o VoidBR Linux.
Um único arquivo Go (`voidbrctl.go`), só stdlib, binário estático.

O runit continua supervisionando (`runsvdir`/`runsv`). O voidbrctl fala direto com o `runsv`
pelos FIFOs `supervise/control` e lê o `supervise/status` binário.

## Instalação

```sh
# binário pronto (x86_64, estático)
sudo install -Dm755 voidbrctl /usr/bin/voidbrctl
sudo voidbrctl setup          # hook de boot em /etc/runit/core-services + diretórios

# ou compilando
go build -trimpath -ldflags "-s -w" -o voidbrctl voidbrctl.go

# opcional: nomes do systemd
sudo ln -s voidbrctl /usr/bin/systemctl
sudo ln -s voidbrctl /usr/bin/journalctl
```

## Uso rápido

```sh
voidbrctl import --dry-run nginx          # prévia do que seria gerado em /etc/sv/nginx
voidbrctl import --enable --now nginx     # converte a unit, habilita e sobe
voidbrctl status                          # visão geral: estado da máquina + árvore de serviços e processos
voidbrctl status nginx                    # estado, PID, memória, processos, últimos logs
voidbrctl logs -u nginx -f                # como journalctl -fu nginx
voidbrctl import --enable backup.timer    # timer + serviço + daemon voidbrctl-timerd
voidbrctl list-timers
voidbrctl calendar "Mon..Fri 09:00"       # como systemd-analyze calendar
```

## Comandos

| Grupo | Comandos |
|---|---|
| Serviços | `status` `start` `stop` `restart` `try-restart` `reload` `reload-or-restart` `kill -s SINAL` `enable [--now]` `disable [--now]` `mask` `unmask` |
| Consulta | `is-active` `is-enabled` `is-failed` `list-units [--all] [--failed]` `list-unit-files` `list-dependencies [--reverse]` `cat` `show [-p PROP] [--value]` |
| Units | `import [--dry-run] [--force] [--enable] [--now] [--restart=POL] [--name=N]` `daemon-reload` `edit` |
| Logs | `logs` / `journal`: `-u` `-f` `-n` `-r` `-b` `--since` `--until` `-g` `-o short\|short-iso\|short-precise\|cat\|json` `-D DIR` `--list` |
| Timers | `timer add NOME QUANDO COMANDO` `timer remove NOME` `list-timers [--all]` `calendar EXPR` `timespan SPAN` `timerd` |
| Sistema | `setup` `poweroff` `reboot` `halt` |

Opções globais: `--no-block`, `--no-color`, `-q`, `--version`, `-h`.

## Mapeamento systemd → runit

| systemd | runit (gerado pelo voidbrctl) |
|---|---|
| `ExecStart=` | `exec chpst … CMD` no `run` |
| `ExecStartPre=` / `ExecStartPost=` | antes do `exec` / em segundo plano após subir |
| `ExecStop=` / `KillSignal=` | `control/t` (runsv chama no lugar do SIGTERM) |
| `ExecReload=` | `control/h` |
| `ExecStopPost=` | `finish` |
| `Restart=no` | `run` grava `o` em `supervise/control` (sv once) |
| `Restart=on-failure/on-success/on-abnormal` | `finish` avalia `$1`/`$2` e grava `d` se não deve reiniciar |
| `RestartSec=` / `SuccessExitStatus=` | `sleep` / função `clean()` no `finish` |
| `Type=oneshot` (+`RemainAfterExit=`) | `start` espera terminar e reporta o código de saída |
| `Type=forking` + `PIDFile=` | `run` vigia o PID; `control/t` mata o daemon |
| `User=` `Group=` `Nice=` `LimitNOFILE=` `LimitNPROC=` | `chpst -u -n -o -p`; demais `Limit*` via `ulimit` |
| `Environment*=` `WorkingDirectory=` `UMask=` `*Directory=` | `export` / `cd` / `umask` / `install -d` |
| `Requires=` `BindsTo=` | `sv check` no `run`; `start` sobe antes; `stop` derruba dependentes; `enable` habilita junto |
| `Wants=` `After=` | `sv check` só se a dependência estiver habilitada |
| journald | `svlogd -tt` em `/var/log/sv/NOME` (+ socklog para serviços nativos) |
| `.timer` | `voidbrctl-timerd` dispara `sv up` no serviço |
| drop-ins, templates `foo@.service`, `%n %i %h …` | suportados |

**Sem root:** `status`, `list-units` e `logs` funcionam como usuário comum (estado lido de `/proc`, já que o runsv cria `supervise/` com permissão 0700); `start`/`stop`/`enable` pedem `sudo`.

**Nomes:** serviços aparecem só pelo nome, como no runit (`nginx`, não `nginx.service`); timers usam o sufixo `.timer` (`voidbrctl status backup.timer`). `nginx.service` também é aceito na entrada.

**Cores:** toda a saída é colorida (help, status, tabelas, logs, timers); desliga com `--no-color`, `NO_COLOR` ou quando a saída não é um terminal. `-l`/`--full` mostra linhas sem cortar na largura do terminal.

**Semântica preservada:** `start` sem `enable` não sobe no próximo boot; `disable` sem `--now` não derruba;
`mask` bloqueia start/enable; estado `failed` vem do código de saída gravado pelo `finish`.
Serviços nativos do Void funcionam sem import (dependências lidas das linhas `sv check X`).

**Sem equivalente no runit (o import avisa):** ativação por socket, `sd_notify`, watchdog,
cgroups (`MemoryMax=`, `CPUQuota=`), sandboxing (`ProtectSystem=`…). `Type=forking` sem `PIDFile=` é recusado.

## Timers (o "cron" do voidbrctl)

Um timer faz um comando rodar sozinho num horário ou intervalo. Quem dispara é o serviço
runit `voidbrctl-timerd`, criado e ligado automaticamente no primeiro timer.

### Jeito rápido: um comando só

```sh
sudo voidbrctl timer add backup daily /usr/local/bin/backup.sh
sudo voidbrctl timer add limpa "every 15min" --user nobody -- rm -rf /tmp/cache
sudo voidbrctl timer add relatorio "Mon..Fri 09:00" --desc "Relatório diário" /opt/rel.sh
sudo voidbrctl timer add aquece "boot 5min" /usr/local/bin/aquece.sh
voidbrctl timer list                   # próximos disparos
voidbrctl logs -u backup               # saída das execuções
sudo voidbrctl timer remove backup     # desliga e apaga tudo que o add criou
```

| QUANDO | Significa |
|---|---|
| `daily`, `hourly`, `weekly`, `monthly`, `yearly` | atalhos (meia-noite, hora cheia, segunda 00:00…) |
| `"Mon..Fri 09:00"`, `"*-*-* 03:00"`, `"*:0/15"` | expressão `OnCalendar=` — teste com `voidbrctl calendar "EXPR"` |
| `"every 15min"`, `"every 2h"` | repete a cada intervalo, contando da última execução |
| `"boot 5min"` | uma vez, 5 minutos após o boot |

Opções do `timer add`: `--user U` (roda como outro usuário), `--desc TEXTO`, `--no-persistent`
(não recupera execuções perdidas com a máquina desligada), `--now` (roda uma vez já),
`--dry-run` (só mostra os arquivos), `--force` (substitui um timer existente).
Use `--` antes do comando se ele tiver opções próprias.

O `timer add` escreve `NOME.service` + `NOME.timer` em `/etc/voidbrctl/system/`, converte o serviço
para `/etc/sv/NOME` (com arquivo `down`, para só rodar quando o timer mandar) e habilita o timer.

### Usando units do systemd

Se já tiver um `.timer` + `.service` (de um pacote ou escrito à mão):
`sudo voidbrctl import --enable backup.timer`.

Suportado: `OnCalendar=` (dias da semana, listas, faixas `..`, passos `/`, `~` fim do mês, fuso,
`daily/weekly/…`), `OnBootSec=`, `OnStartupSec=`, `OnActiveSec=`, `OnUnitActiveSec=`,
`OnUnitInactiveSec=` (aproximado), `Persistent=`, `RandomizedDelaySec=`.

## Caminhos (sobrescrevíveis por ambiente)

| Variável | Padrão |
|---|---|
| `VOIDBRCTL_SVDIR` | `/etc/sv` |
| `VOIDBRCTL_RUNDIR` | `/var/service` |
| `VOIDBRCTL_LOGDIR` | `/var/log/sv` |
| `VOIDBRCTL_CONFDIR` | `/etc/voidbrctl` |
| `VOIDBRCTL_STATEDIR` | `/var/lib/voidbrctl` |
| `VOIDBRCTL_RUNTIMEDIR` | `/run/voidbrctl` |
| `VOIDBRCTL_SOCKLOGDIR` | `/var/log/socklog` |
| `VOIDBRCTL_UNITPATH` | busca extra de units |
| `SVWAIT` | timeout de start/stop (7 s) |

## Histórico

- **v0.1.3** — `voidbrctl timer add/remove/list`: cria timers num comando só, sem escrever units.
- **v0.1.2** — projeto renomeado de voidctl para **voidbrctl** (binário, fonte, serviço `voidbrctl-timerd`, variáveis `VOIDBRCTL_*`, diretórios `/etc/voidbrctl`, `/var/lib/voidbrctl`, `/run/voidbrctl`).
- **v0.1.1** — `status` sem root (via `/proc`); `status` sem argumento com árvore de serviços/processos; saída toda colorida; nomes sem `.service`; correção do `socklog-unix` aparecendo como `auto-restart`.
- **v0.1.0** — versão inicial.
