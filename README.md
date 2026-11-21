# Arara Agent Gateway

Servidor open source que gerencia instâncias isoladas de agentes de programação (Claude Code, Codex e Google Antigravity) numa única máquina Linux. Funciona no modelo da Evolution API: um servidor central, uma chave global (`apikey`), N instâncias independentes com conexões próprias e uma dashboard administrativa.

```bash
cp .env.example .env   # preencha API_KEY e CREDENTIALS_ENCRYPTION_KEY
scripts/build-runtime.sh
docker compose up -d --build
```

Abra `http://localhost:3000` e entre com a `API_KEY`.

## Por que existe

O AC-WEB (PWA e Desktop) precisa oferecer agentes de programação que rodam na nuvem, com isolamento entre sessões e sem manter uma máquina por usuário. O gateway resolve só a execução: cria e gerencia instâncias sob demanda, liga o container quando necessário, roda o CLI escolhido com streaming e desliga o container quando fica ocioso. Quem decide qual instância pertence a qual usuário é o backend integrador (por exemplo, o `ac-services`).

## Modelo Evolution API

- Um servidor central gerencia quantas instâncias independentes o operador precisar.
- Autenticação única pelo cabeçalho `apikey`, com a chave definida no `.env`.
- Cada instância é autocontida: container, volume persistente, conexões dos três agentes, conexão GitHub, repositórios clonados e histórico.
- API REST documentada (`docs/openapi.yaml`) como contrato principal.
- Dashboard administrativa separada, que consome a mesma API.
- O login por URL e código faz o papel do QR code da Evolution API.

Diferenças: cada instância é um container que executa código, então o isolamento é obrigatório e as instâncias desligam por inatividade. O progresso chega por SSE em vez de webhooks.

## Diferenças em relação ao Cezar

O [Cezar](https://github.com/open-mercato/cezar) é um cockpit completo para um desenvolvedor, com workflows, filas, variantes, automações, Jira/Linear e custos, escrito em Node. O gateway usa o mesmo jeito de dirigir os CLIs, mas é muito menor:

- Go e SQLite num binário de cerca de 20 MB de RAM.
- Múltiplas instâncias isoladas numa única máquina.
- Só instâncias, execuções, histórico curto, conexões e GitHub.
- Nenhuma interface para o usuário final: isso é papel do backend e do frontend que consomem o gateway.

## Arquitetura

```text
AC-WEB PWA / Desktop
        │ (sessão do AC-WEB)
   backend AC-WEB ────── HTTPS + apikey ──────▶ Arara Agent Gateway (Go)
   (mapeia usuário ↔ instância)                   ├─ API REST + SSE
                                                  ├─ dashboard (embutida)
                                                  ├─ instâncias / inatividade
                                                  ├─ GitHub (por instância)
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
DOCKER_GID=$(stat -c %g /var/run/docker.sock)
```

O gateway não inicia sem `API_KEY` (32+ caracteres) e `CREDENTIALS_ENCRYPTION_KEY` (32 bytes). Guarde a chave de cifra: sem ela, os tokens e as chaves salvas ficam ilegíveis.

### Deploy com Docker Compose

Local:

```bash
scripts/build-runtime.sh
docker compose up -d --build
docker compose logs -f gateway
```

Atrás de um proxy reverso Traefik numa rede Docker externa (por exemplo `microservices_network`):

```bash
scripts/build-runtime.sh
docker compose -f docker-compose.yml -f docker-compose.proxy.yml up -d --build
```

O `scripts/build-runtime.sh` constrói a imagem de runtime (`RUNTIME_IMAGE` do `.env`, padrão `arara-agent-runtime:local`, cerca de 2 GB) com as versões mais recentes de Claude Code, Codex e Antigravity. Rode-o antes de cada deploy (no Komodo, no `pre_deploy`). Se o build falhar, ele mantém a imagem atual. Instâncias ligadas passam a usar a imagem nova quando são desligadas e ligadas de novo.

O Compose:
- sobe o gateway como usuário sem privilégio, com rootfs somente leitura;
- no modo local, publica a porta só em `127.0.0.1`; no modo proxy (`docker-compose.proxy.yml`), remove a porta do host e expõe o serviço via labels Traefik.

### Sem Docker Compose

```bash
scripts/build-runtime.sh
go build -o arara-agent-gateway ./cmd/gateway
set -a; . ./.env; set +a
./arara-agent-gateway
```

### Configuração

Todas as variáveis estão em [`.env.example`](.env.example). As principais:

| Variável | Padrão | Para quê |
|---|---|---|
| `API_KEY` | — | Chave global de acesso (dashboard e backend integrador) |
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

## Autenticação

Toda chamada (exceto `GET /api/health` e `GET /api/openapi.yaml`) precisa do cabeçalho `apikey`:

```bash
curl -H "apikey: $API_KEY" http://localhost:3000/api/instances
```

- A dashboard pede a chave na abertura e a mantém só na memória da aba.
- Não existe cadastro nem controle de usuários finais no gateway: quem gerencia usuários e associa cada usuário à sua instância é o backend integrador.
- Nunca envie a `API_KEY` ao navegador ou ao app Desktop.

## Instâncias

```bash
H=(-H "apikey: $API_KEY" -H "Content-Type: application/json")

ID=$(curl -s "${H[@]}" -X POST http://localhost:3000/api/instances -d '{"name":"Principal"}' | jq -r .instance.id)

curl -s "${H[@]}" http://localhost:3000/api/instances
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/start
curl -s "${H[@]}" http://localhost:3000/api/instances/$ID/status
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/stop
curl -s "${H[@]}" -X DELETE "http://localhost:3000/api/instances/$ID?confirm=$ID"
```

- `POST /api/instances` sempre cria uma nova instância (sem limite de 1 por usuário) e não liga container.
- `start` e `stop` são idempotentes.
- Excluir exige `confirm` igual ao ID e apaga container, volume, conexões e histórico da instância.

## Conexão GitHub por instância

Registre um GitHub App (recomendado) ou um OAuth App:
- **GitHub App**: permissões `Contents: Read and write` e `Metadata: Read-only`, com token de usuário expirável.
- **OAuth App**: escopo `repo`.

Em `GITHUB_REDIRECT_URL`, coloque uma página do backend integrador. Ela recebe `code` e `state` e repassa para o gateway na instância correspondente.

```bash
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/github/connect
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/github/callback -d '{"code":"…","state":"…"}'
curl -s "${H[@]}" http://localhost:3000/api/instances/$ID/github/repos
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/repos -d '{"full_name":"octo/app","select":true}'
curl -s "${H[@]}" "http://localhost:3000/api/instances/$ID/repos/changes?full_name=octo%2Fapp&agent=codex&include_diff=true"
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/repos/octo/app/push -d '{"agent":"codex"}'
curl -s "${H[@]}" -X DELETE http://localhost:3000/api/instances/$ID/github
```

- O token fica cifrado no banco, vinculado à instância, e nunca entra no container.
- Cada agente trabalha no próprio worktree, na branch `arara/<agente>`.
- O push só aceita branches `arara/*`.

## Conexão dos três CLIs

| Agente | Login de conta | Chave de API |
|---|---|---|
| Claude Code | `claude auth login`: abre a URL e cola o código | `ANTHROPIC_API_KEY` |
| Codex | `codex login --device-auth`: abre a URL e digita o código | `CODEX_API_KEY` |
| Google Antigravity | `agy` (Google OAuth): abre a URL e cola o código | `GEMINI_API_KEY` |

```bash
curl -s "${H[@]}" -X PUT http://localhost:3000/api/instances/$ID/agents/antigravity/api-key -d '{"api_key":"AIza…"}'
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/agents/antigravity/login
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/agents/antigravity/login/input -d '{"code":"…"}'
curl -s "${H[@]}" http://localhost:3000/api/instances/$ID/agents
```

- O login usa o fluxo oficial de cada CLI. O usuário só vê a URL, o código e um campo de uma linha, nunca um terminal.
- Logins de conta ficam nos arquivos do próprio CLI, dentro do volume da instância. Chaves de API ficam cifradas no gateway e só entram na execução.

## Execução de prompts e streaming

```bash
RUN=$(curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/agents/claude/run \
  -d '{"prompt":"Liste os arquivos do projeto","repo":"octo/app"}' | jq -r .run.id)

curl -N "${H[@]}" http://localhost:3000/api/instances/$ID/runs/$RUN/stream
curl -s "${H[@]}" -X POST http://localhost:3000/api/instances/$ID/runs/$RUN/cancel
```

O stream SSE envia os eventos `status`, `session`, `delta` (texto), `reasoning` (raciocínio do Claude Code e do Codex), `tool` e `done` (resultado final). Para continuar uma conversa, envie o `session_id` de uma execução anterior. Os três agentes podem rodar ao mesmo tempo na mesma instância, um de cada.

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
- Para endurecimento adicional em produção, pode-se usar `CONTAINER_RUNTIME=runsc` (gVisor) e regras na cadeia `DOCKER-USER` bloqueando acesso à rede do host.
- O gateway acessa o socket do Docker, o que equivale a root no host. Exponha a API só por HTTPS ou rede privada.
- Volumes locais não têm cota de disco: monitore o uso.

## Integração com o AC-WEB

O contrato completo para o backend do AC-WEB (PWA e Desktop) está em [`docs/ac-web-integration.md`](docs/ac-web-integration.md). Ele cobre autenticação, mapeamento de instâncias, estados, erros, GitHub, login dos CLIs e repasse do streaming.

## Desenvolvimento

```bash
gofmt -l .
go vet ./...
go test -race ./...
./scripts/e2e-docker.sh
```

- O `e2e-docker.sh` sobe o gateway com diretório, porta e rede temporários e testa contra o Docker real. Ele verifica isolamento, logins dos três CLIs, execuções em paralelo, cancelamento, inatividade, reinício e exclusão, usando chaves inválidas, sem gastar créditos.
- `E2E_SKIP_PROVIDERS=1` pula as chamadas aos provedores, como no CI.
- Regras para contribuir: [`AGENTS.md`](AGENTS.md).

## Licença

MIT. Veja [`LICENSE`](LICENSE).
