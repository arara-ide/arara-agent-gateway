# Decisões técnicas

Registro das escolhas de arquitetura, com motivo e alternativas descartadas. Como o código não tem comentários, as explicações ficam aqui.

## Estudo de referência

- **Evolution API**: inspirou o modelo de um servidor central com instâncias independentes, a chave global no cabeçalho `apikey`, a API REST como contrato principal e a dashboard separada como cliente da API. Não reproduzimos o modelo de token por instância, os webhooks, as filas (RabbitMQ, SQS) nem o Prisma com vários bancos. O consumidor aqui é um único backend, e o streaming por SSE resolve o que os webhooks resolvem lá.
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

## Autenticação e escopo

- `API_KEY` administrativa, comparada em tempo constante, como na Evolution API.
- `INTEGRATION_API_KEY` opcional, para o backend do AC-WEB. Ela sempre exige `X-User-Id` e nunca enxerga recursos de outro usuário. Mesmo que vaze, o estrago fica limitado ao usuário enviado no cabeçalho, e a chave nunca dá acesso à listagem global.
- Recursos de outro usuário respondem `404`, não `403`, para não revelar se existem.
- A dashboard guarda a chave só numa variável JavaScript. Nada vai para `localStorage`, cookie ou URL. Por isso o stream usa `fetch` com cabeçalho em vez de `EventSource`, que não aceita cabeçalhos.

## Credenciais

- Segredos no banco (tokens GitHub e chaves de API dos provedores): AES-256-GCM com `CREDENTIALS_ENCRYPTION_KEY` e o ID do dono como dado associado. Um segredo copiado para outro usuário não abre.
- Chaves de API dos provedores entram como variável de ambiente só no `exec` da execução. Não ficam no container nem no volume.
- Logins de conta (Claude e Codex) ficam nos arquivos oficiais dos CLIs dentro do volume (`~/.claude/.credentials.json`, `~/.codex/auth.json`). Os CLIs precisam ler esses arquivos, então não há como cifrá-los no gateway. Mitigação: um volume por usuário, sem montagem cruzada, e recomendação de disco cifrado no host.
- O Codex é configurado com `cli_auth_credentials_store = "file"`, porque o container não tem keyring.
- Tokens GitHub nunca entram no volume. Cada operação Git recebe `http.extraheader` por variáveis `GIT_CONFIG_*`, que valem só para aquele processo.

## Login dos CLIs

- **Claude Code**: `claude auth login --claudeai` num `exec` com TTY. O gateway extrai a URL oficial e aceita só uma linha de código pelo endpoint `login/input`. Não existe terminal livre.
- **Codex**: `codex login --device-auth`. O gateway extrai a URL e o código do dispositivo.
- **Antigravity**: só chave de API (`GEMINI_API_KEY` com `--settings '{"modelProvider":"gemini"}'`, flag listada no `agy --help` 1.5.12). O login Google exige Secret Service/D-Bus no Linux. Além disso, os termos adicionais do Antigravity (seção 6) proíbem acessar o serviço por software de terceiros com o OAuth do Antigravity.
- **Termos**: a documentação da Anthropic restringe produtos de terceiros que oferecem o login do claude.ai. A forma claramente suportada para uso hospedado é a chave de API própria do usuário. O login de conta ficou disponível porque usa o fluxo oficial do CLI dentro do ambiente isolado do próprio usuário. Antes de oferecê-lo a terceiros, o operador deve conferir os termos da Anthropic e da OpenAI.

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
- Containers não equivalem a VMs: os agentes executam código arbitrário. Para vários clientes, use `CONTAINER_RUNTIME=runsc` (gVisor, funciona sem KVM) e uma máquina dedicada, separada de bancos e serviços de produção.
- A rede de saída fica aberta, porque os CLIs precisam das APIs dos provedores, do npm e do GitHub. Para bloquear o acesso à rede do host e a metadados de nuvem, crie regras na cadeia `DOCKER-USER` do host.
- Volumes locais do Docker não têm cota de disco. Monitore o uso ou use um driver de volume com cota.
- O gateway precisa do socket do Docker, que equivale a root no host. Proteja o processo e não o exponha sem TLS.
