# voidbrctl v0.1.4 — manual completo

`voidbrctl` é um comando no estilo `systemctl` + `journalctl` para o **runit** do VoidBR Linux.
É um único programa em Go, sem dependências e com binário estático.

---

## 1. A ideia em 30 segundos

São três peças, e cada uma tem um papel só:

| Peça | Papel |
|---|---|
| **runit** (`runsvdir` + um `runsv` por serviço) | **executa** e vigia os serviços, como sempre fez |
| **voidbrctl** (o comando) | **dá as ordens** ao runsv e **lê** o estado, os logs e as configurações |
| **voidbrctl-timerd** (um serviço runit) | **relógio**: na hora marcada, manda o runsv subir um serviço |

O voidbrctl não substitui nada no runit. Ele faz o mesmo que o `sv`, só que:
- fala direto com o runsv: escreve no FIFO `supervise/control` e lê o arquivo binário `supervise/status`;
- mostra tudo com os termos e as cores do systemctl;
- sabe converter units do systemd (`.service`, `.timer`) em serviços runit.

---

## 2. Onde ficam as coisas

| Caminho | O que é | Quem cria |
|---|---|---|
| `/etc/sv/NOME/` | definição do serviço runit (`run`, `finish`, `log/run`…) | pacote do Void, ou `import` / `timer add` |
| `/var/service/NOME` | link = serviço **habilitado** (o runsvdir supervisiona) | `enable` / `start` |
| `/etc/sv/NOME/down` | arquivo vazio = "não suba sozinho" | `start` sem enable, `disable`, timers |
| `/etc/sv/NOME/voidbrctl.meta` | anotações do voidbrctl (origem, tipo, dependências…) | `import` |
| `/etc/sv/NOME/voidbrctl.service` | cópia da unit original, para consulta | `import` |
| `/etc/voidbrctl/system/` | suas units `.service`/`.timer` (têm prioridade) | você, `timer add`, `edit` |
| `/etc/voidbrctl/timers/NOME.timer` | timer importado | `import`, `timer add` |
| `/etc/voidbrctl/timers/enabled/` | links = timers **habilitados** | `enable NOME.timer` |
| `/etc/voidbrctl/masked/NOME` | link para `/dev/null` = serviço **mascarado** | `mask` |
| `/var/lib/voidbrctl/runtime/NOME` | marca de "subiu sem enable" | `start`, `disable` |
| `/var/lib/voidbrctl/timers/NOME` | hora do último disparo do timer | `voidbrctl-timerd` |
| `/run/voidbrctl/NOME.exit` | código de saída da última execução | script `finish` gerado |
| `/var/log/sv/NOME/current` | log do serviço (svlogd) | `log/run` gerado |
| `/etc/runit/core-services/99-voidbrctl.sh` | limpeza no boot | `setup` |

---

## 3. Instalação

```sh
sudo install -Dm755 voidbrctl-v0.1.4 /usr/bin/voidbrctl
sudo voidbrctl setup
```

**`setup`** faz duas coisas:
1. Cria `/etc/runit/core-services/99-voidbrctl.sh`. O runit executa esse arquivo no estágio 1 do boot, e ele roda `voidbrctl boot-cleanup` (ver §10).
2. Cria as pastas `/etc/voidbrctl/{system,timers/enabled,masked}` e `/var/lib/voidbrctl/{runtime,timers}`.

Opcional, para usar os nomes do systemd:
```sh
sudo ln -s voidbrctl /usr/bin/systemctl
sudo ln -s voidbrctl /usr/bin/journalctl   # chamado assim, vira "voidbrctl logs"
```

---

## 4. Opções globais (valem em qualquer comando e posição)

| Opção | Efeito |
|---|---|
| `--no-color` | desliga as cores (também desligam com `NO_COLOR=1` ou quando a saída não é um terminal) |
| `--color` | força as cores mesmo em pipe |
| `-q`, `--quiet` | não imprime as mensagens informativas ("Created symlink…") |
| `--no-block` | `start`/`stop`/`restart` mandam a ordem e **não esperam** o resultado |
| `--version` | mostra a versão |
| `-h`, `--help`, `help` | ajuda |
| (nenhum comando) | o mesmo que `list-units` |

Nomes: um serviço é só o nome (`nginx`). `nginx.service` também é aceito. Um timer leva o sufixo `.timer` (`backup.timer`), para não ser confundido com o serviço `backup`.

---

## 5. Serviços

### `status [NOME…]`

**Sem nome**, é a visão geral da máquina:
1. conta os serviços em `/var/service` e quantos estão ativos e com falha;
2. mostra `State: running` (verde) ou `degraded` (amarelo, se algum falhou);
3. mostra desde quando a máquina está ligada (lê o `btime` de `/proc/stat`);
4. monta a árvore: PID 1 → `runsvdir` → cada `runsv NOME` → processos de cada serviço, com a bolinha colorida.

**Com nome**, mostra o detalhe do serviço:
1. `Loaded:` diz onde o serviço está (`/etc/sv/NOME`), se está habilitado (`enabled`/`disabled`/`masked`/`static`), se tem `down` e de qual unit veio;
2. `Active:` dá o estado (§11) e desde quando;
3. `Process:` mostra a última saída (código ou sinal), quando o serviço está parado;
4. `Main PID`, `Tasks` (threads), `Memory` (RSS somado da árvore) e `Processes` (árvore);
5. mostra as últimas 10 linhas de log.

| Opção | Efeito |
|---|---|
| `-n N`, `--lines N` | quantas linhas de log mostrar (0 = nenhuma) |
| `-l`, `--full` | não corta as linhas na largura do terminal |

Código de saída: 0 se ativo, 3 se não, 4 se o serviço não existe.
Sem root, o estado vem de `/proc` (§12).

### `start NOME…`
1. Confere se o serviço existe e não está mascarado.
2. Sobe antes as dependências: as **obrigatórias** (`Requires`), abortando se alguma falhar, e as **opcionais** (`Wants`), só avisando se falharem.
3. Se o serviço **não está habilitado**, cria um link "temporário" em `/var/service`, junto com o arquivo `down` e a marca em `/var/lib/voidbrctl/runtime/`. Assim ele roda agora mas **não volta no próximo boot**, igual ao `systemctl start`. Depois espera até 12 s o runsvdir assumir o serviço, porque ele varre a pasta a cada 5 s.
4. Manda `u` ao runsv, que é o mesmo que `sv up`.
5. Espera o resultado:
   - **serviço normal**: espera ficar `run`, por até 7 s (`SVWAIT`) ou pelo `TimeoutStartSec` da unit. Se houver script `check`, roda até ele dar OK. Por fim confere, 300 ms depois, se o processo não morreu logo após subir;
   - **oneshot**: espera **terminar**, por até 5 min ou `TimeoutStartSec`, e informa se falhou e com qual código.

### `stop NOME…`
1. Antes, para os serviços que **dependem** deste e estão rodando.
2. Manda `d` ao runsv (= `sv down`).
3. Espera parar, por até 7 s ou `TimeoutStopSec`. Se não parar, manda SIGKILL e espera mais 3 s.
4. Se era um start "temporário", remove o link, o `down` e a marca.

### `restart NOME…`
Se o serviço está parado, faz `start`. Se está rodando, manda `t`, `c` e `u` (TERM, CONT e "quero no ar") e espera o **novo** processo subir.

### `try-restart NOME…` (ou `condrestart`)
Reinicia só se já estiver rodando. Se estiver parado, não faz nada.

### `reload NOME…`
Manda `h` ao runsv. Se o serviço tiver `control/h` (gerado a partir do `ExecReload=`), o runsv executa esse script; senão envia SIGHUP ao processo.

### `reload-or-restart NOME…`
Faz `reload` se o serviço tiver `control/h`, e `restart` se não tiver.

### `kill [-s SINAL] NOME…`
Envia um sinal ao processo principal (padrão: TERM).
TERM, HUP, INT, QUIT, USR1, USR2, ALRM, KILL, CONT e STOP vão pelo runsv. Os outros sinais vão direto ao PID.
Aceita o nome (`-s HUP`, `-s SIGHUP`) ou o número (`-s 1`).

### `enable [--now] NOME…`
1. Se o serviço estava rodando "temporário", só remove o `down` e a marca: ele passa a ser permanente.
2. Senão, cria o link `/var/service/NOME → /etc/sv/NOME`.
3. Avisa se existir um `down` que você mesmo criou, porque nesse caso o serviço não sobe no boot.
4. Habilita também as dependências **obrigatórias**. Sem isso o serviço ficaria preso no boot esperando por elas.
5. Com `--now`, faz `start` em seguida.

### `disable [--now] NOME…`
- **Sem `--now`, com o serviço rodando**: ele **continua rodando**, mas não volta no boot. Por baixo, cria `down` e a marca de temporário; o hook do boot remove o link depois. É o mesmo comportamento do `systemctl disable`.
- **Sem `--now`, com o serviço parado**: remove o link de `/var/service`.
- **Com `--now`**: faz `stop` e remove o link.

### `mask NOME…` / `unmask NOME…`
- `mask` cria `/etc/voidbrctl/masked/NOME → /dev/null` e faz `disable`. A partir daí, `start` e `enable` recusam o serviço.
- `unmask` remove a máscara.

### `is-active` / `is-enabled` / `is-failed NOME…`
Servem para scripts: imprimem o estado e devolvem o código de saída.

| Comando | Imprime | Sai com 0 se… |
|---|---|---|
| `is-active` | `active`, `inactive`, `failed`, `activating`… | ativo (senão 3) |
| `is-enabled` | `enabled`, `disabled`, `masked`, `static` | `enabled` ou `static` (senão 1) |
| `is-failed` | o estado | `failed` (senão 3) |

Com `-q`, não imprime nada; vale só o código:
```sh
voidbrctl -q is-active nginx && echo no ar
```

### `list-units` (ou `list`, `ls`)
Tabela SERVICE / LOAD / ACTIVE / SUB / DESCRIPTION.

| Opção | Efeito |
|---|---|
| (nenhuma) | só os habilitados (em `/var/service`) |
| `-a`, `--all` | todos os de `/etc/sv` |
| `--failed` | só os com falha |
| `--state=X` | filtra por estado (`active`, `running`, `failed`, `dead`…) |
| `--no-legend` | sem cabeçalho e rodapé (para scripts) |
| `-t timer` | mostra os timers (= `list-timers`) |

### `list-unit-files`
Lista todos os serviços de `/etc/sv` **e** os timers, com o estado (`enabled`, `disabled`, `masked`, `static`) e a origem: `runit` quando é nativo do Void, ou `voidbrctl (arquivo.service)` quando foi importado.

### `list-dependencies [--reverse] NOME`
Mostra a árvore de dependências com as bolinhas de estado.
- Nos serviços importados, as dependências vêm dos metadados (`Requires`/`Wants`).
- Nos **nativos do Void**, o voidbrctl lê o `run` e procura linhas como `sv check dbus`.
- `--reverse` inverte a pergunta: mostra quem depende deste serviço.

### `cat NOME`
Mostra a unit original, se o serviço foi importado, e depois todos os scripts do serviço: `run`, `finish`, `check`, `conf`, `control/*`, `log/run`.
Com `cat NOME.timer`, mostra o timer.

### `show [-p PROP[,PROP]] [--value] NOME…`
Imprime propriedades no formato `CHAVE=valor`, para scripts:
`Id`, `Description`, `LoadState`, `ActiveState`, `SubState`, `UnitFileState`, `MainPID`, `ActiveEnterTimestamp`, `FragmentPath`, `NormallyUp`, `ExecMainStatus`, `ExecMainExitTimestamp`.
Nos importados, também: `SourcePath`, `Type`, `Restart`, `Requires`, `Wants`, `After`, `User`, `TriggeredBy`.
- `-p MainPID,ActiveState` filtra as propriedades.
- `--value` imprime só o valor.

### `edit NOME`
Abre o editor (`$VISUAL`, `$EDITOR` ou `vi`):
- **serviço importado**: copia a unit para `/etc/voidbrctl/system/` (se ainda não estiver lá), abre o editor e, ao salvar, **gera de novo** o `/etc/sv/NOME`. Se ele estiver rodando, avisa para fazer `restart`;
- **serviço nativo**: abre o `/etc/sv/NOME/run`;
- **`NOME.timer`**: abre o timer e avisa o timerd.

---

## 6. Units do systemd

### `import [opções] UNIT…`

Converte uma unit do systemd num serviço runit.

**Onde procura a unit**, nesta ordem: `/etc/voidbrctl/system`, `/etc/systemd/system`, `/usr/local/lib/systemd/system`, `/usr/lib/systemd/system`, `/lib/systemd/system`. Também aceita um caminho direto (`./foo.service`).
Junta os drop-ins (`NOME.service.d/*.conf`) e entende templates (`foo@bar.service` usa `foo@.service`).

| Opção | Efeito |
|---|---|
| `-n`, `--dry-run` | só **mostra** os arquivos que seriam criados; não grava nada |
| `-f`, `--force` | sobrescreve um `/etc/sv/NOME` que não foi criado pelo voidbrctl |
| `--enable` | habilita depois de importar |
| `--now` | habilita e sobe |
| `--restart=POL` | força a política de reinício (`always`, `on-failure`, `no`…) |
| `--name=N` | usa outro nome no runit |

**O que ele gera em `/etc/sv/NOME/`:**

| Na unit | No runit |
|---|---|
| `ExecStart=` | a última linha do `run`: `exec chpst … COMANDO` |
| `ExecStartPre=` | linhas antes do `exec` (se uma falhar, aborta) |
| `ExecStartPost=` | roda em segundo plano, 1 s depois de subir |
| `User=` `Group=` `SupplementaryGroups=` | `chpst -u usuário:grupo` |
| `Nice=` `LimitNOFILE=` `LimitNPROC=` | `chpst -n` / `-o` / `-p` |
| `LimitCORE/MEMLOCK/STACK/AS=` | `ulimit` |
| `Environment=` / `EnvironmentFile=` | `export` / `set -a; . arquivo` |
| `WorkingDirectory=` `UMask=` `RootDirectory=` | `cd` / `umask` / `chpst -/` |
| `RuntimeDirectory=` `StateDirectory=` `CacheDirectory=` `LogsDirectory=` | `install -d` com o dono certo |
| `StandardOutput=null / file: / append:` | redirecionamento no `run` |
| `Requires=` `BindsTo=` | `sv check DEP \|\| exit 1`: o runit tenta de novo até a dependência subir |
| `Wants=` `After=` | o mesmo `sv check`, mas só se a dependência estiver habilitada |
| `Restart=no` (padrão do systemd) | o `run` avisa o runsv: "rode uma vez só" |
| `Restart=on-failure/on-success/on-abnormal` | o `finish` olha o código de saída e decide se reinicia |
| `RestartSec=` | `sleep` no `finish` antes de reiniciar |
| `SuccessExitStatus=` | códigos considerados sucesso |
| `ExecStop=` / `KillSignal=` | `control/t` (o runsv executa no lugar do SIGTERM) |
| `ExecReload=` | `control/h` |
| `ExecStopPost=` | dentro do `finish` |
| `Type=oneshot` (+`RemainAfterExit=`) | roda os comandos em sequência; com `RemainAfterExit`, fica "ativo" até o `stop` |
| `Type=forking` + `PIDFile=` | o `run` vigia o PID do arquivo e o `control/t` mata o daemon |
| log (journald) | `log/run` com `svlogd -tt /var/log/sv/NOME` |

Também grava o `finish` (que registra o código de saída em `/run/voidbrctl/NOME.exit`), o `voidbrctl.meta` e a cópia `voidbrctl.service`.

**Ele avisa quando** encontra o que o runit não tem: `WatchdogSec`, `ProtectSystem`, `PrivateTmp`, `MemoryMax`, `CPUQuota` e parecidos. Essas diretivas são ignoradas.

**Ele recusa:** `.socket` (não existe ativação por socket no runit) e `Type=forking` sem `PIDFile=`.

Com um `.timer`, importa o timer e também o `.service` que ele aciona (§8).

### `daemon-reload`
Para cada serviço importado (os que têm `voidbrctl.meta`), lê de novo a unit de origem e gera os scripts de novo. Se existir uma cópia em `/etc/voidbrctl/system`, ela tem prioridade. Também confere os timers e avisa o timerd.
Não reinicia nada: se algo mudou num serviço rodando, avisa para fazer `restart`.

---

## 7. Logs

### `logs` (ou `journal`, `journalctl`)

**De onde lê:**
- **com `-u NOME`**, nesta ordem:
  1. diretórios do svlogd: `/var/log/sv/NOME/`, o diretório indicado no `log/run` do serviço e `/var/log/NOME/` (se tiver `current`);
  2. **arquivos de log do próprio programa**: `/var/log/NOME/*.log` (e o `.log.1` rotacionado) e `/var/log/NOME.log`. É o caso do nginx (`/var/log/nginx/access.log` e `error.log`). Cada linha aparece com o arquivo de origem, como `nginx[error.log]`, e a data é lida da própria linha (formatos do nginx/apache, ISO e syslog);
  3. se não achar nada, o **socklog** (`/var/log/socklog/everything`), filtrado pelo nome do programa. É por onde passam os serviços nativos do Void que usam `vlogger`.

  Se uma pasta de log for só do root (como `/var/log/nginx`, com permissão 0750), avisa que precisa de `sudo`. Se não achar nada, lista onde procurou;
- **sem `-u`**: tudo de `/var/log/sv/*`, mais o socklog.

Lê os arquivos antigos (`@….s`) e o `current`, entende os três formatos de hora do svlogd e junta tudo **em ordem cronológica**.

| Opção | Efeito |
|---|---|
| `-u NOME` | só esse serviço (pode repetir: `-u nginx -u php-fpm`) |
| `-f`, `--follow` | acompanha ao vivo (mostra as 10 últimas e depois as novas); percebe a rotação do svlogd |
| `-n N` | só as últimas N linhas (`-n all` = todas) |
| `-r`, `--reverse` | as mais novas primeiro |
| `-S`, `--since T` / `-U`, `--until T` | intervalo de tempo |
| `-b`, `--boot` | só desde o boot atual |
| `-g REGEX` | filtra pelo texto; ignora maiúsculas se a regex estiver toda em minúsculas |
| `--case-sensitive` | a busca do `-g` diferencia maiúsculas de minúsculas |
| `-o FORMATO` | `short` (padrão), `short-iso`, `short-precise`, `cat` (só a mensagem), `json` |
| `-D DIR` | lê um diretório svlogd qualquer |
| `--list` | lista de onde está lendo |
| `--no-pager` | não abre o `less` |
| `-e` | abre o `less` já no fim |
| `--no-hostname` | tira o nome da máquina das linhas |

Formatos de hora aceitos em `--since`/`--until`: `today`, `yesterday`, `now`, `"1h ago"`, `-30min`, `"2026-09-29 14:00"`, `"14:00"`.

Cores: hora em cinza, máquina em azul, serviço em magenta. Linhas com *error/fail/fatal/panic* saem em vermelho e com *warn* em amarelo.
Num terminal, abre o `less` (ou `$PAGER`).

---

## 8. Timers (o "cron" do voidbrctl)

### `timer add NOME QUANDO [opções] [--] COMANDO [ARGS…]`

**Passo a passo:**
1. Confere o nome, o QUANDO e se o comando existe (procura no PATH e usa o caminho completo).
2. Recusa se já existir um serviço nativo ou um timer com esse nome (a menos que use `--force`).
3. Escreve `/etc/voidbrctl/system/NOME.service` (`Type=oneshot`, `ExecStart=COMANDO`) e `NOME.timer`.
4. Faz o `import` do timer e do serviço. O serviço vira `/etc/sv/NOME` **com `down`**, porque só roda quando o timer mandar.
5. Habilita o timer: cria o link em `timers/enabled/` e o link `/var/service/NOME`. O runsv fica esperando.
6. Na primeira vez, cria e sobe o serviço `voidbrctl-timerd`.
7. Mostra o `status` do timer, com o próximo disparo.

| QUANDO | Vira na unit | Significa |
|---|---|---|
| `daily` `hourly` `weekly` `monthly` `yearly` `quarterly` | `OnCalendar=` | atalhos |
| `"Mon..Fri 09:00"`, `"*-*-* 03:00"`, `"*:0/15"`, `"Sat,Sun 10:30 UTC"` | `OnCalendar=` | calendário do systemd |
| `"every 15min"` | `OnBootSec=` + `OnUnitActiveSec=` | a cada 15 min, contando da última execução |
| `"boot 5min"` | `OnBootSec=` | uma vez, 5 min depois do boot |

| Opção | Efeito |
|---|---|
| `-u`, `--user U` | roda o comando como o usuário U |
| `-d`, `--desc TEXTO` | descrição |
| `--no-persistent` | **não** recupera o disparo perdido com a máquina desligada |
| `--now` | roda uma vez agora, além de agendar |
| `-n`, `--dry-run` | só mostra os dois arquivos |
| `-f`, `--force` | substitui um timer que já existe |
| `--` | separa o comando, quando ele tem opções próprias |

### `timer remove NOME…`
Desabilita o timer e apaga `timers/NOME.timer`. Para e apaga o serviço que foi **criado para o timer** (`/etc/sv/NOME` e o link), apaga os arquivos que o `timer add` escreveu em `/etc/voidbrctl/system` e apaga a hora do último disparo.
Nunca apaga um serviço nativo do Void.

### `timer list` / `list-timers [--all]`
Tabela NEXT (próximo disparo), LEFT (quanto falta), LAST (último disparo), PASSED, TIMER, SERVICE.
Com `--all`, inclui os timers desabilitados. Avisa se o `voidbrctl-timerd` não estiver rodando.

### `enable NOME.timer` / `disable NOME.timer` / `start` / `stop`
Habilitam ou desabilitam um timer que já foi importado (`start`/`stop` num `.timer` fazem o mesmo que `enable`/`disable`).

### `status NOME.timer`
Mostra o próximo disparo (`Trigger`), o último (`Triggered`), a expressão de calendário normalizada e qual serviço ele aciona.

### `calendar [--iterations=N] EXPR…`
Testa uma expressão `OnCalendar=` sem criar nada. Mostra a forma normalizada, o próximo disparo (na hora local e em UTC) e quanto falta. Com `--iterations`, mostra os N próximos.

### `timespan SPAN…`
Testa um intervalo (`"1h 30min"`, `90`, `2d12h`) e mostra o valor em microssegundos e por extenso.

### `timerd` (o relógio; roda como serviço runit, você não chama à mão)
1. Grava a hora em que começou em `/run/voidbrctl/timerd.start`.
2. Lê os timers habilitados e calcula o próximo disparo de cada um, levando em conta o calendário, o boot, a última execução e o `RandomizedDelaySec`.
3. Dorme até o disparo mais próximo, acordando no máximo a cada 1 min.
4. Na hora de disparar:
   - grava a hora em `/var/lib/voidbrctl/timers/NOME`;
   - manda `u` ao runsv do serviço (= `sv up`). Se o serviço ainda estiver rodando, **ignora** esse disparo.
5. Com **`Persistent=true`**, compara a hora do último disparo com o calendário. Se perdeu algum horário (máquina desligada), dispara na hora.
6. Com SIGHUP, relê os timers. `enable`, `disable`, `import` e `daemon-reload` mandam esse sinal.
7. Tudo o que ele faz vai para `voidbrctl logs -u voidbrctl-timerd`.

---

## 9. Sistema

| Comando | Faz |
|---|---|
| `poweroff`, `reboot`, `halt` | chama o comando do sistema com o mesmo nome |
| `setup` | ver §3 |

---

## 10. No boot

1. **Estágio 1**: o runit executa `99-voidbrctl.sh` → `voidbrctl boot-cleanup`, que:
   - remove os links dos serviços que você subiu com `start` sem `enable`, ou desabilitou sem `--now`, e o `down` que o voidbrctl criou para eles;
   - apaga `/run/voidbrctl`.
2. **Estágio 2**: o runsvdir sobe o que está em `/var/service`:
   - serviços habilitados: sobem normalmente;
   - serviços de timer: ficam parados (por causa do `down`);
   - `voidbrctl-timerd`: sobe e passa a controlar os horários.
3. Um serviço importado com `Requires=` fica tentando (`sv check … || exit 1`) até a dependência estar no ar.

---

## 11. Estados: o que cada um significa e como é descoberto

| Bolinha | Estado | Quando |
|---|---|---|
| 🟢 `●` | `active (running)` | o runsv diz `run` e há um processo principal |
| 🟢 `●` | `active (exited)` | oneshot com `RemainAfterExit=yes` que já rodou |
| 🟡 `●` | `activating (auto-restart)` | o serviço acabou de morrer com erro e o runsv está subindo de novo |
| 🟡 `●` | `deactivating` | recebeu a ordem de parar e ainda não parou |
| 🔴 `×` | `failed` | parado, e a última saída foi com erro (código ≠ 0 ou sinal que não seja TERM/INT/HUP/PIPE) |
| ⚪ `○` | `inactive (dead)` | parado, sem erro |

| Habilitação | Significa |
|---|---|
| `enabled` | tem link em `/var/service` e sobe no boot |
| `disabled` | não tem link, ou está rodando só de forma temporária |
| `static` | serviço de timer: quem sobe é o timer |
| `masked` | bloqueado com `mask` |

---

## 12. Sem root

O runsv cria `supervise/` com permissão 0700, então um usuário comum não consegue ler o estado. Nesse caso o voidbrctl:
1. procura em `/proc` o processo `runsv NOME`;
2. pega o filho dele que não é o logger (svlogd/vlogger…) como processo principal;
3. usa `/proc/PID/stat` para saber desde quando ele está rodando.

`status`, `list-units` e `logs` funcionam normalmente, com uma nota no `status` dizendo que o estado foi lido de `/proc`. `start`, `stop`, `enable` e parecidos pedem `sudo`.

---

## 13. Variáveis de ambiente

| Variável | Padrão |
|---|---|
| `VOIDBRCTL_SVDIR` | `/etc/sv` |
| `VOIDBRCTL_RUNDIR` | `/var/service` |
| `VOIDBRCTL_LOGDIR` | `/var/log/sv` |
| `VOIDBRCTL_CONFDIR` | `/etc/voidbrctl` |
| `VOIDBRCTL_UNITDIR` | `/etc/voidbrctl/system` |
| `VOIDBRCTL_TIMERDIR` | `/etc/voidbrctl/timers` |
| `VOIDBRCTL_STATEDIR` | `/var/lib/voidbrctl` |
| `VOIDBRCTL_RUNTIMEDIR` | `/run/voidbrctl` |
| `VOIDBRCTL_SOCKLOGDIR` | `/var/log/socklog` |
| `VOIDBRCTL_UNITPATH` | pastas extras onde procurar units (separadas por `:`) |
| `SVWAIT` | tempo de espera de start/stop (7 s) |
| `VISUAL` / `EDITOR` | editor do `edit` |
| `PAGER` / `SYSTEMD_PAGER` | paginador dos `logs` |
| `NO_COLOR` | desliga as cores |

---

## 14. O que não existe no runit (e portanto no voidbrctl)

Ativação por socket, `sd_notify`/watchdog, cgroups (limite de memória/CPU, `KillMode=control-group`), sandboxing (`ProtectSystem=`, `PrivateTmp=`…) e `StartLimitBurst=`.
O runit reinicia um serviço que cai para sempre, sem limite de tentativas.

---

## Histórico

- **v0.1.4** — `logs -u` também lê os arquivos de log do próprio programa (`/var/log/NOME/*.log`, ex.: nginx), com aviso claro de falta de permissão e de onde procurou.
- **v0.1.3** — `timer add/remove/list`: cria timers num comando só; README virou manual completo.
- **v0.1.2** — renomeado de voidctl para voidbrctl.
- **v0.1.1** — `status` sem root; árvore no `status`; saída colorida; nomes sem `.service`.
- **v0.1.0** — versão inicial.
