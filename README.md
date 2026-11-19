# Arara Agent Gateway

Servidor open source que gerencia instâncias isoladas de agentes de programação (Claude Code, Codex e Google Antigravity) numa única máquina Linux. Funciona como uma "Evolution API para agentes": um servidor central, uma instância por usuário, API com chave e uma dashboard administrativa.

```bash
cp .env.example .env   # preencha API_KEY e CREDENTIALS_ENCRYPTION_KEY
docker compose up -d --build
```

Abra `http://localhost:3000` e entre com a `API_KEY`.

## Por que existe

O AC-WEB (PWA e Desktop) precisa oferecer agentes de programação que rodam na nuvem, com o login de cada usuário e sem manter uma máquina por pessoa. O gateway resolve só a parte de execução: liga um container quando o usuário pede, roda o CLI escolhido, transmite a resposta e desliga o container quando ninguém usa. Ele é independente e pode servir qualquer outro backend.

## Semelhanças com a Evolution API

- Um servidor central gerencia muitas instâncias independentes.
- Autenticação pelo cabeçalho `apikey`, com a chave definida no `.env`.
- API REST documentada (`docs/openapi.yaml`) como contrato principal.
- Dashboard administrativa separada, que usa a mesma API.
- A sessão de cada instância fica persistida: lá o WhatsApp, aqui os logins dos CLIs. O login por URL e código faz o papel do QR code.

Diferenças: cada instância é um container que executa código, então o isolamento é obrigatório e as instâncias desligam por inatividade. Não há webhooks: o progresso chega por SSE.

## Diferenças em relação ao Cezar

O [Cezar](https://github.com/open-mercato/cezar) é um cockpit completo para um desenvolvedor, com workflows, filas, variantes, automações, Jira/Linear e custos, escrito em Node. O gateway usa o mesmo jeito de dirigir os CLIs, mas é muito menor:

- Go e SQLite num binário de cerca de 20 MB de RAM.
- Multiusuário, com um container por usuário.
- Só instâncias, execuções, histórico curto, conexões e GitHub.
- Nenhuma interface para o usuário final: isso é papel do AC-WEB.

## Arquitetura

```text
AC-WEB PWA / Desktop
        │ (sessão do AC-WEB)
   backend AC-WEB ── HTTPS + apikey + X-User-Id ──▶ Arara Agent Gateway (Go)
                                                     ├─ API REST + SSE
                                                     ├─ dashboard (embutida)
                                                     ├─ instâncias / inatividade
                                                     ├─ GitHub
                                                     └─ SQLite (DATA_DIR/gateway.db)
                                                            │ socket do Docker
                                   ┌────────────────────────┼────────────────────────┐
                              container A              container B              container C
                         claude · codex · agy     claude · codex · agy     claude · codex · agy
                            volume aag-<id>          volume aag-<id>          volume aag-<id>
```

- Uma imagem de runtime compartilhada (`runtime/Dockerfile`) traz os três CLIs.
- Cada instância tem um container e um volume próprio em `/home/agent`, com logins, repositórios e sessões.
- Os CLIs só rodam durante uma execução ou um login, via `docker exec`. Um container ligado e ocioso usa cerca de 2,5 MB de RAM.
- Desligar remove o container e mantém o volume. Ligar cria outro container com o mesmo volume.

Detalhes e motivos das escolhas: [`docs/decisions.md`](docs/decisions.md).

## Instalação

Requisitos: Linux com Docker Engine 24+ (ou Docker Desktop para testes) e, para compilar fora do Docker, Go 1.27+.

```bash
git clone https://github.com/arara-ide/arara-agent-gateway.git
cd arara-agent-gateway
cp .env.example .env
```

Edite o `.env`:

```bash
API_KEY=$(openssl rand -hex 32)
CREDENTIALS_ENCRYPTION_KEY=$(openssl rand -hex 32)
INTEGRATION_API_KEY=$(openssl rand -hex 32)
DOCKER_GID=$(stat -c %g /var/run/docker.sock)
```

O gateway não inicia sem `API_KEY` (32+ caracteres) e `CREDENTIALS_ENCRYPTION_KEY` (32 bytes). Guarde a chave de cifra: sem ela, os tokens e as chaves salvas ficam ilegíveis.

### Deploy com Docker Compose

```bash
docker compose up -d --build
docker compose logs -f gateway
```

O Compose:
- constrói a imagem de runtime (`arara-agent-runtime:local`, cerca de 2 GB);
- sobe o gateway como usuário sem privilégio, com rootfs somente leitura;
- publica a porta só em `127.0.0.1`. Coloque um proxy reverso com HTTPS na frente.

### Sem Docker Compose

```bash
docker build -f runtime/Dockerfile -t arara-agent-runtime:local .
go build -o arara-agent-gateway ./cmd/gateway
set -a; . ./.env; set +a
./arara-agent-gateway
```

### Configuração

Todas as variáveis estão em [`.env.example`](.env.example). As principais:

| Variável | Padrão | Para quê |
|---|---|---|
| `API_KEY` | — | Chave administrativa (dashboard e acesso total) |
| `INTEGRATION_API_KEY` | vazio | Chave do backend AC-WEB. Exige `X-User-Id` em toda chamada. |
| `CREDENTIALS_ENCRYPTION_KEY` | — | Cifra de tokens GitHub e chaves de API |
| `IDLE_TIMEOUT_SECONDS` | `300` | Tempo sem atividade até desligar |
| `IDLE_CHECK_INTERVAL_SECONDS` | `15` | Frequência da verificação |
| `AUTO_START` | `false` | Liga a instância ao receber um prompt |
| `MAX_ACTIVE_INSTANCES` | `10` | Instâncias ligadas ao mesmo tempo |
| `MAX_AGENTS_PER_INSTANCE` | `3` | Execuções simultâneas por instância |
| `CONTAINER_MEMORY` / `CONTAINER_CPUS` / `CONTAINER_PIDS` | `4g` / `2` / `512` | Limites de cada container |
| `CONTAINER_RUNTIME` | vazio | `runsc` para usar gVisor |
| `RUN_TIMEOUT_SECONDS` | `3600` | Tempo máximo de uma execução |
| `SESSION_RETENTION_DAYS` | `30` | Retenção dos arquivos de sessão dos CLIs |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` / `GITHUB_REDIRECT_URL` | vazio | Integração GitHub, opcional |

## Autenticação administrativa

Toda chamada (exceto `GET /api/health` e `GET /api/openapi.yaml`) precisa do cabeçalho `apikey`:

```bash
curl -H "apikey: $API_KEY" http://localhost:3000/api/instances
```

- A dashboard pede a chave na abertura e a mantém só na memória da aba.
- Não existe cadastro nem senha de usuários no gateway: quem autentica os usuários finais é o AC-WEB.
- Com `X-User-Id`, qualquer chave passa a enxergar só os recursos daquele usuário. A `INTEGRATION_API_KEY` sempre exige esse cabeçalho.
- Nunca envie nenhuma das chaves ao navegador ou ao app Desktop.

## Instâncias

```bash
H=(-H "apikey: $INTEGRATION_API_KEY" -H "X-User-Id: acweb_user_123" -H "Content-Type: application/json")

curl -s "${H[@]}" -X POST http://localhost:3000/api/instances -d '{"name":"Principal"}'
ID=$(curl -s "${H[@]}" http://localhost:3000/api/users/acweb_user_123/instance | jq -r .instance.id)

curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/start
curl -s "${H[@]}" http://localhost:3000/api/instances/$ID/status
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/stop
curl -s "${H[@]}" -X DELETE "http://localhost:3000/api/instances/$ID?confirm=$ID"
```

- Criar é idempotente e não liga container.
- `start` e `stop` também são idempotentes.
- Excluir exige `confirm` igual ao ID e apaga container, volume e histórico.

## Conexão GitHub por usuário

Registre um GitHub App (recomendado) ou um OAuth App:
- **GitHub App**: permissões `Contents: Read and write` e `Metadata: Read-only`, com token de usuário expirável.
- **OAuth App**: escopo `repo`.

Em `GITHUB_REDIRECT_URL`, coloque uma página do AC-WEB. Ela recebe `code` e `state` e repassa para o gateway com o usuário da sessão.

```bash
curl -s "${H[@]}" -X POST http://localhost:3000/api/users/acweb_user_123/github/connect
curl -s "${H[@]}" -X POST http://localhost:3000/api/users/acweb_user_123/github/callback -d '{"code":"…","state":"…"}'
curl -s "${H[@]}" http://localhost:3000/api/users/acweb_user_123/github/repos
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/repos -d '{"full_name":"octo/app","select":true}'
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/repos/octo/app/push -d '{"agent":"codex"}'
curl -s "${H[@]}" -X DELETE http://localhost:3000/api/users/acweb_user_123/github
```

- O token fica cifrado no banco e nunca entra no container.
- Cada agente trabalha no próprio worktree, na branch `arara/<agente>`.
- O push só aceita branches `arara/*`.

## Conexão dos três CLIs

| Agente | Login de conta | Chave de API |
|---|---|---|
| Claude Code | `claude auth login`: abre a URL e cola o código | `ANTHROPIC_API_KEY` |
| Codex | `codex login --device-auth`: abre a URL e digita o código | `CODEX_API_KEY` |
| Google Antigravity | Não suportado (veja abaixo) | `GEMINI_API_KEY` |

```bash
curl -s "${H[@]}" -X PUT http://localhost:3000/api/users/acweb_user_123/agents/antigravity/api-key -d '{"api_key":"AIza…"}'
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/agents/claude/login
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/agents/claude/login/input -d '{"code":"…"}'
curl -s "${H[@]}" http://localhost:3000/api/instances/$ID/agents
```

- O login usa o fluxo oficial do CLI. O usuário só vê a URL, o código e um campo de uma linha, nunca um terminal.
- Logins de conta ficam nos arquivos do próprio CLI, dentro do volume da instância. Chaves de API ficam cifradas no gateway e só entram na execução.
- O Antigravity só aceita chave de API: o login Google dele exige keyring local, e os termos do Antigravity proíbem usar o OAuth dele por software de terceiros.
- Para oferecer o login de conta do Claude ou do Codex a terceiros, confira antes os termos da Anthropic e da OpenAI. A chave de API do próprio usuário é o caminho claramente suportado para uso hospedado.

## Execução de prompts e streaming

```bash
RUN=$(curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/agents/claude/run \
  -d '{"prompt":"Liste os arquivos do projeto","repo":"octo/app"}' | jq -r .run.id)

curl -N "${H[@]}" http://localhost:3000/api/instances/$ID/runs/$RUN/stream
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/runs/$RUN/cancel
```

O stream SSE envia os eventos `status`, `session`, `delta` (texto), `tool` e `done` (resultado final). Para continuar uma conversa, envie o `session_id` de uma execução anterior. Os três agentes podem rodar ao mesmo tempo na mesma instância, um de cada.

## Histórico

`GET /api/instances/{id}/history` devolve as 20 execuções mais recentes. O limite é aplicado no banco: a 21ª apaga a mais antiga. Arquivos de sessão dos CLIs com mais de `SESSION_RETENTION_DAYS` dias são apagados quando a instância liga.

## Desligamento automático

- Sem execuções ou logins ativos e sem interação por `IDLE_TIMEOUT_SECONDS`, a instância é desligada e libera a memória.
- Uma tarefa longa nunca é interrompida pelo tempo de inatividade: a contagem recomeça quando ela termina.
- A instância só volta a ligar com `start`, ou com um prompt quando `AUTO_START=true`.

## Persistência

| Onde | O quê |
|---|---|
| `DATA_DIR/gateway.db` | instâncias, histórico, conexões e segredos cifrados |
| volume `aag-<id>` | logins de conta dos CLIs, sessões e repositórios |

Tudo sobrevive a desligamento manual ou automático, a reinício do container, do gateway e do servidor. Ao iniciar, o gateway reconcilia o estado com o Docker, encerra processos órfãos e marca como `interrupted` as execuções que estavam em andamento.

## Limites e segurança

- Containers sem privilégio:
  - usuário 1000, `cap_drop ALL` e `no-new-privileges`;
  - rootfs somente leitura;
  - limites de memória, CPU e PIDs;
  - rede própria sem comunicação entre containers;
  - sem socket do Docker.
- **Containers não isolam como VMs.** Os agentes executam código arbitrário. Em produção multiusuário:
  - use `CONTAINER_RUNTIME=runsc` (gVisor) numa máquina dedicada, sem bancos nem serviços de produção;
  - bloqueie o acesso à rede do host na cadeia `DOCKER-USER`;
  - cifre o disco do host.
- O gateway acessa o socket do Docker, o que equivale a root no host. Exponha a API só por HTTPS ou rede privada.
- Volumes locais não têm cota de disco: monitore o uso.

## Integração com o AC-WEB

O contrato completo para o backend do AC-WEB (PWA e Desktop) está em [`docs/ac-web-integration.md`](docs/ac-web-integration.md). Ele cobre autenticação, associação de usuários, estados, erros, GitHub, login dos CLIs e repasse do streaming.

## Desenvolvimento

```bash
gofmt -l .
go vet ./...
go test -race ./...
./scripts/e2e-docker.sh
```

- O `e2e-docker.sh` sobe o gateway com diretório, porta e rede temporários e testa contra o Docker real. Ele verifica isolamento, logins, execuções em paralelo, cancelamento, inatividade, reinício e exclusão, usando chaves inválidas, sem gastar créditos.
- `E2E_SKIP_PROVIDERS=1` pula as chamadas aos provedores, como no CI.
- Regras para contribuir: [`AGENTS.md`](AGENTS.md).

## Licença

MIT. Veja [`LICENSE`](LICENSE).
