# Decisões técnicas

Registro das escolhas de arquitetura, com motivo e alternativas descartadas. Como o código não tem comentários, as explicações ficam aqui.

## Estudo de referência

- **Evolution API**: inspirou o modelo de um servidor central com N instâncias independentes sob uma única chave global no cabeçalho `apikey`, a API REST como contrato principal, conexões autocontidas por instância e a dashboard separada como cliente da API. Quem mapeia qual instância pertence a qual usuário é o backend integrador (`ac-services`). Não reproduzimos token por instância, webhooks, filas (RabbitMQ, SQS) nem Prisma com vários bancos: o streaming por SSE resolve o progresso das execuções.
- **Cezar** (open-mercato/cezar): confirmou como cada CLI é dirigido em modo não interativo.
  - Claude Code: `claude -p --output-format stream-json`, com `--resume`.
  - Codex: Cezar usa o `codex app-server`. Aqui usamos `codex exec --json`, que é mais simples, sem um processo JSON-RPC de longa duração.
  - Antigravity: `agy --output-format stream-json`, com `--conversation`.

  Também serviu de referência para os worktrees por tarefa e as fixtures de saída real. Ficaram de fora workflows, automações, variantes, filas, MCP privado, custo e uso, Jira e Linear, além do servidor Node. As fixtures em `internal/agent/testdata` são capturas reais dos CLIs (Claude Code 2.1.328, Codex 0.170.0, agy 1.5.12), com dados pessoais removidos.

## Arquitetura

| Decisão | Motivo | Alternativas descartadas |
|---|---|---|
| Um binário Go com API, dashboard, gerenciador de inatividade e GitHub | Pouca RAM (cerca de 20 MB de RSS medidos com 50 instâncias cadastradas), um processo só, deploy simples | Microserviços, Node, filas |
| SQLite (`modernc.org/sqlite`, sem CGO) com uma conexão | Persistência sem servidor extra. Uma conexão serializa as escritas e evita `SQLITE_BUSY`. | PostgreSQL, Redis |
| Cliente Docker próprio sobre o socket Unix | O SDK oficial traz dezenas de dependências. Usamos só 12 endpoints. | `github.com/docker/docker/client` |
| Container removido ao desligar e recriado ao ligar | Tudo que importa fica no volume. Recriar aplica imagem e limites novos e libera RAM e disco. | `docker pause` (proibido pelo requisito; mantém a memória), container parado e reaproveitado |
| Um volume nomeado `aag-<id>` em `/home/agent` | Logins, sessões e repositórios sobrevivem a desligamentos e reinícios. Na primeira montagem, o volume herda os arquivos da imagem. | Bind mount no host (expõe caminhos do host) |
| Processos dos CLIs só via `docker exec`, por execução ou login | Nada fica rodando em segundo plano. Um container ocioso usa poucos MB. | Daemon dentro do container (como o Terragon), `codex app-server` permanente |
| `aag-run` e `aag-kill` na imagem | O Docker não cancela um `exec`. O wrapper grava o PID, e o `aag-kill` encerra a árvore de processos (TERM e depois KILL). O stdin é duplicado no fd 3 porque o `dash` liga processos em segundo plano a `/dev/null`. | Fechar a conexão do exec, que deixa o processo vivo |
| Concorrência: uma execução por agente e `MAX_AGENTS_PER_INSTANCE` por instância | Cada agente tem pasta e worktree próprios, então não há conflito de arquivos | Fila por instância |
| Contador de atividade em memória mais `last_activity_at` no banco | O desligamento por inatividade usa a mesma trava por instância que `Acquire`, então não há corrida entre o início de uma tarefa e o desligamento. Depois de reiniciar o gateway, execuções antigas viram `interrupted` e o relógio recomeça. | Timer por instância |
| Um ticker global de inatividade | Custo constante e baixo, mesmo com centenas de instâncias | Goroutine por instância |
| SSE com buffer de 5.000 eventos por execução e retenção de 2 minutos | Permite reconexão com `Last-Event-ID` sem guardar eventos no banco | WebSocket, eventos persistidos |
| Overlay `docker-compose.proxy.yml` separado do `docker-compose.yml` | Permite uso local imediato sem redes externas e deploy atrás do Traefik (rede externa e labels) sem conflito de porta entre produção e sandbox | Exigir rede externa no Compose padrão |
| Imagem de runtime construída por `scripts/build-runtime.sh`, fora do Compose | O deploy resolve as versões mais recentes de Claude Code, Codex (npm) e Antigravity (manifesto oficial) e passa como `--build-arg`, então o cache só cai quando sai versão nova. Se o build falhar, mantém a imagem atual; sem imagem, usa os padrões do Dockerfile. Fora do Compose, a stack não fica com um container encerrado que o Komodo marca como Unhealthy. O CI usa os padrões do Dockerfile para ser reprodutível. Instâncias ligadas pegam a imagem nova ao serem recriadas. | Serviço `runtime` no Compose com `command: ["true"]`, `ARG ...=latest` (o cache do Docker nunca atualiza) |

## Autenticação e escopo

- Uma única `API_KEY` global, comparada em tempo constante, como na Evolution API.
- O gateway não armazena usuários nem restringe instâncias por usuário: todas as instâncias são administradas pelo portador da `API_KEY`, e o backend consumidor (`ac-services`) faz o mapeamento e a autorização dos usuários finais.
- A dashboard guarda a chave só numa variável JavaScript. Nada vai para `localStorage`, cookie ou URL. Por isso o stream usa `fetch` com cabeçalho em vez de `EventSource`, que não aceita cabeçalhos.

## Credenciais

- Todas as conexões (GitHub e chaves de API dos provedores) pertencem à instância (`PRIMARY KEY (instance_id, provider)` com `ON DELETE CASCADE`).
- Segredos no banco são cifrados com AES-256-GCM usando `CREDENTIALS_ENCRYPTION_KEY` e o `instance_id` como dado associado. Um segredo copiado para outra instância não abre.
- Chaves de API dos provedores entram como variável de ambiente só no `exec` da execução. Não ficam no container nem no volume.
- Logins de conta ficam nos arquivos oficiais dos CLIs dentro do volume da instância (`~/.claude/.credentials.json`, `~/.codex/auth.json`, `~/.gemini/antigravity-cli/`). Mitigação: um volume por instância, sem montagem cruzada, e recomendação de disco cifrado no host.
- O Codex é configurado com `cli_auth_credentials_store = "file"`, porque o container não tem keyring. O `agy` faz fallback automático para armazenamento em arquivo quando o keyring não está disponível no Linux.
- Tokens GitHub nunca entram no volume. Cada operação Git recebe `http.extraheader` por variáveis `GIT_CONFIG_*`, que valem só para aquele processo.

## Login dos CLIs

- **Claude Code**: `claude auth login --claudeai` num `exec` com TTY. O gateway extrai a URL oficial e aceita só uma linha de código pelo endpoint `login/input`. Não existe terminal livre.
- **Codex**: `codex login --device-auth`. O gateway extrai a URL e o código do dispositivo.
- **Google Antigravity**: `agy` num `exec` com TTY (`SSH_CONNECTION` e `TERM=xterm-256color`, janela 60×1000 para a URL não quebrar linha). O gateway seleciona automaticamente a primeira opção (`Google OAuth`) ao detectar `Select login method:`, extrai a URL `https://accounts.google.com/o/oauth2/auth…`, envia o código único recebido em `login/input` e encerra o TUI assim que a autenticação conclui (por texto de confirmação ou sondagem via `agy --output-format json models`) ou falha. O logout usa `agy -i /logout` em TTY e encerra ao detectar confirmação ou estado deslogado. Também aceita `GEMINI_API_KEY` com `--settings '{"modelProvider":"gemini"}'`.

## Git

- Clone em `~/work/repos/<owner>/<name>` e um worktree por agente em `~/work/worktrees/<owner>/<name>/<agent>`, na branch `arara/<agent>`. O worktree é persistente, porque as sessões do Claude são ligadas à pasta.
- O push só aceita `arara/*`. O agente faz commits locais, mas não tem credencial para enviar. O envio passa sempre pelo endpoint de push.
- `git merge --ff-only` no clone principal durante o sync. O clone principal nunca recebe alterações dos agentes.

## Retenção

- Histórico: 20 execuções por instância, aplicado na mesma transação do `INSERT`.
- Sessões dos CLIs: `aag-prune` roda a cada início e apaga arquivos com mais de `SESSION_RETENTION_DAYS` dias (padrão 30) nas pastas de sessão, log e cache dos três CLIs. Também corta para 1 MB os históricos acima de 5 MB. Uma sessão apagada não pode ser retomada: a execução com aquele `session_id` falha.
- Saída de cada execução: resposta limitada a 256 KB no banco, linha de evento até 8 MB, `stderr` guardado só nos últimos 16 KB.

## Isolamento e limites conhecidos

- Container de instância com usuário 1000, `cap_drop: ALL`, `no-new-privileges`, rootfs somente leitura, `/tmp` em tmpfs de 1 GB, limites de memória, CPU e PIDs, `Init`, sem socket do Docker e rede bridge com `enable_icc=false`.
- Para endurecimento adicional, pode-se ativar `CONTAINER_RUNTIME=runsc` (gVisor) e regras na cadeia `DOCKER-USER` bloqueando acesso à rede do host e a metadados de nuvem.
- Volumes locais do Docker não têm cota de disco. Monitore o uso ou use um driver de volume com cota.
- O gateway precisa do socket do Docker, que equivale a root no host. Proteja o processo e não o exponha sem TLS.
