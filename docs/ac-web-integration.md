# Contrato de integração com o AC-WEB

Este documento descreve como o AC-WEB PWA e o AC-WEB Desktop usam o Arara Agent Gateway. Os dois usam o mesmo contrato, sempre por meio do backend do AC-WEB (`ac-services`). A referência formal de cada rota está em [`openapi.yaml`](openapi.yaml), também servida em `GET /api/openapi.yaml`.

## 1. Quem fala com quem

```text
PWA / Desktop ──(sessão do AC-WEB)──> backend AC-WEB (ac-services) ──(HTTPS + apikey)──> Gateway
```

- O navegador e o app Desktop **nunca** chamam o gateway direto e **nunca** recebem a `API_KEY`.
- O gateway segue o modelo da Evolution API: uma única chave global (`apikey`) com acesso a todas as instâncias.
- O `ac-services` é quem autentica o usuário final, guarda o mapeamento `userId ↔ instanceId` (um ou mais, conforme a regra de negócio do produto) e verifica se o usuário da sessão é dono da instância antes de repassar qualquer chamada ao gateway.

## 2. Autenticação

| Cabeçalho | Valor |
|---|---|
| `apikey` | `API_KEY` configurada no gateway |

- Toda rota sob `/api/*` (exceto `/api/health` e `/api/openapi.yaml`) exige `apikey` válida (`401 unauthorized` quando ausente ou inválida).
- Em produção, publique o gateway só em HTTPS (proxy reverso) ou na rede privada `microservices_network` entre o `ac-services` e o gateway.

Exemplo de cliente no backend (TypeScript/NestJS):

```ts
const GATEWAY_URL = process.env.AGENT_GATEWAY_URL!;
const GATEWAY_KEY = process.env.AGENT_GATEWAY_KEY!;

async function gateway(method: string, path: string, body?: unknown) {
  const res = await fetch(`${GATEWAY_URL}/api${path}`, {
    method,
    headers: {
      apikey: GATEWAY_KEY,
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return null;
  const data = await res.json();
  if (!res.ok) throw Object.assign(new Error(data.error.code), { status: res.status, code: data.error.code });
  return data;
}
```

## 3. Organização de instâncias no `ac-services`

- O gateway não impõe limite de 1 instância por usuário nem conhece usuários finais.
- `POST /api/instances` com `{ "name": "opcional" }` (ou `{}`) sempre cria uma nova instância e devolve `201 Created` com `{ "instance": { "id": "inst_…", ... } }`.
- `GET /api/instances/metrics` devolve, para cada instância, `storage_bytes` (tamanho do volume via `docker system df`), e `cpu_percent`, `memory_bytes` e `memory_limit_bytes` só quando ligada (senão `null`). Usado pelo painel admin do AC-WEB; a consulta é lenta (até 10 s) e não deve ser chamada em polling curto.
- O `ac-services` salva o `instance.id` associado ao usuário no MongoDB e valida a propriedade em todas as rotas expostas ao cliente.

Fluxo recomendado ao abrir o módulo de agentes:

1. O `ac-services` busca a instância do usuário no próprio banco; se ainda não existir, chama `POST /api/instances` e salva o `instance.id`.
2. Chama `GET /api/instances/{id}/status` para mostrar o estado da instância, dos três agentes e do GitHub.
3. Se `instance.state` for `stopped`, mostra o botão "Ligar", que chama `POST /api/instances/{id}/start`.

## 4. Estados

### Instância (`instance.state`)

| Estado | Significado |
|---|---|
| `stopped` | Sem container. O volume com logins, repositórios e sessões continua guardado. |
| `starting` | Container sendo criado e iniciado. |
| `running` | Pronta para logins e execuções. |
| `stopping` | Encerrando execuções e o container. |
| `error` | Falha ao ligar. Detalhe em `instance.last_error`. Tente `start` de novo. |

Outros campos úteis: `last_activity_at`, `idle_stop_at` (quando a instância vai desligar sozinha, presente só se ligada e ociosa), `running_agents` e `active_runs`.

### Conexões (`connections[]`, `agents[]`, `github`)

| `status` | Significado |
|---|---|
| `connected` | Pronto para uso. `account` mostra e-mail, login GitHub, `Google` ou a chave mascarada (`AIza…0000`). |
| `disconnected` | Desconectado. |
| `expired` | Login de conta que deixou de valer (token revogado ou vencido). Peça novo login. |
| `error` | Chave de API recusada pelo provedor. Peça uma chave nova. |
| `setup_required` | Nunca configurado. |

`method` vale `account` (login oficial do CLI), `api_key` (chave do próprio usuário) ou `oauth` (GitHub).

## 5. Conexão dos agentes (por instância)

| Agente | `account` | `api_key` | Observação |
|---|---|---|---|
| `claude` | Sim: `claude auth login`. A pessoa abre a URL e cola o código. | `ANTHROPIC_API_KEY` | A conta fica nos arquivos do CLI dentro do volume da instância. |
| `codex` | Sim: `codex login --device-auth`. A pessoa abre a URL e digita o código. | `CODEX_API_KEY` | O login por código de dispositivo precisa estar habilitado na conta ChatGPT. |
| `antigravity` | Sim: `agy` (Google OAuth). A pessoa abre a URL e cola o código. | `GEMINI_API_KEY` com `modelProvider: gemini` | Sem keyring no container, o CLI usa armazenamento em arquivo dentro do volume. |

### Chave de API (vale para os três)

```http
PUT /api/instances/{id}/agents/{agent}/api-key
{ "api_key": "..." }
```

A chave fica cifrada no gateway (vinculada ao `instance_id`) e só entra como variável de ambiente durante cada execução. Ela nunca é devolvida. Para remover, use `DELETE` na mesma rota.

### Login de conta (Claude Code, Codex e Google Antigravity)

A instância precisa estar ligada.

1. `POST /api/instances/{id}/agents/{agent}/login` devolve `login`:
   ```json
   { "id": "login_…", "status": "waiting_for_user", "url": "https://…", "code": "ABCD-12345", "needs_input": false, "expires_at": "…" }
   ```
2. Mostre `url` como link para o usuário abrir no navegador dele. Se houver `code`, mostre-o (Codex).
3. Se `needs_input` for `true` (Claude Code e Google Antigravity), mostre um campo para o código exibido depois do login e envie com `POST …/login/input` `{ "code": "…" }`. O campo aceita uma linha só, sem espaços. Nada além disso chega ao terminal.
4. Se o envio do código ainda devolver uma sessão pendente ou se usar o código de dispositivo, consulte `GET …/login` até `status` virar `succeeded`, `failed` ou `cancelled`. Em `failed`, `error` vale `login_timeout`, `login_failed` ou `not_authenticated`. A sessão concluída descarta URL e código.
5. Para cancelar, use `DELETE …/login`. Para sair da conta, use `POST …/logout`. Durante o login, alterar a chave ou sair da conta devolve `409 login_in_progress`. Depois de enviar um código, o cancelamento do login também devolve esse erro até o resultado.

A sessão de login dura até 10 minutos e mantém a instância ligada enquanto estiver aberta.

## 6. GitHub (por instância)

O GitHub funciona por instância, com GitHub App (recomendado, token de usuário com validade curta e refresh) ou com OAuth App. O gateway usa `state` de uso único (10 minutos), vinculado à instância, e PKCE S256.

`GITHUB_REDIRECT_URL` aponta para uma página **do AC-WEB**, não do gateway. Assim, a conclusão acontece dentro da sessão autenticada do usuário, e o `ac-services` encaminha `code` e `state` para a instância correta daquele usuário.

1. Usuário clica em "Conectar GitHub" no PWA/Desktop.
2. O backend chama `POST /api/instances/{id}/github/connect`, recebe `authorize_url` e redireciona o navegador para lá.
3. O GitHub redireciona para `GITHUB_REDIRECT_URL?code=…&state=…` no AC-WEB.
4. O backend do AC-WEB valida o usuário da sessão, recupera a instância dele e chama `POST /api/instances/{id}/github/callback` com `{ "code": "…", "state": "…" }`.
5. A resposta traz `connection.status = connected` e `account` (login GitHub). `400 github_invalid_state` indica link expirado, já usado ou de outra instância.

Outras operações:

| Ação | Rota |
|---|---|
| Estado (valida o token no GitHub) | `GET /api/instances/{id}/github` |
| Listar repositórios acessíveis | `GET /api/instances/{id}/github/repos?page=1` |
| Clonar ou atualizar na instância | `POST /api/instances/{id}/repos` `{ "full_name": "owner/name", "select": true }` |
| Repositórios clonados | `GET /api/instances/{id}/repos` |
| Alterações no worktree | `GET /api/instances/{id}/repos/changes?full_name=owner%2Fname&agent=codex&include_diff=true` |
| Escolher o repositório padrão | `PATCH /api/instances/{id}` `{ "selected_repo": "owner/name" }` |
| Enviar o trabalho de um agente | `POST /api/instances/{id}/repos/{owner}/{name}/push` `{ "agent": "codex" }` |

- Cada agente trabalha num worktree próprio, na branch `arara/<agente>`. O push só aceita branches `arara/*` e nunca envia para a branch padrão.
- A consulta de alterações usa Git local, sem credenciais, fetch ou renovação de inatividade. Instância desligada retorna `409 instance_not_running`. A resposta usa `Cache-Control: no-store` e sempre inclui `has_changes` e `can_push`, ambos falsos se o worktree ainda não existe.
- `has_changes` inclui alterações no worktree e commits após a base comum. A base compara o `HEAD` com a referência local de `origin/arara/<agente>` ou, antes do primeiro envio, com o `HEAD` do clone principal. `can_push` indica commits capazes de avançar essa referência local. O push continua validando permissões e estado atual no GitHub.
- Com `include_diff=true`, a resposta pode incluir `diff` com até 256 KiB da saída do Git, `untracked` com até 200 caminhos de arquivos novos e `truncated` se parte da saída foi omitida. Os arquivos novos aparecem pelo caminho.
- O token GitHub não fica no volume. O gateway o injeta como cabeçalho temporário em cada `clone`, `fetch` e `push`. Os agentes fazem commits locais e o push passa sempre pelo gateway.
- `409 github_expired` indica que o usuário precisa reconectar.
- Não há desconexão manual: a conexão só sai quando o token expira ou é revogado no GitHub (estado `expired`) ou quando a instância é apagada (`DELETE /api/instances/{id}`), que revoga o token no GitHub e remove a conexão.

## 7. Execução de prompts e streaming

```http
POST /api/instances/{id}/agents/{agent}/run
{ "prompt": "Explique o README", "session_id": "opcional", "model": "opcional", "repo": "owner/name opcional" }
```

- `202` devolve `run` (`status: running`) e `stream_url`.
- Sem `repo`, a execução usa `selected_repo`. Sem nenhum dos dois, ela roda numa pasta de rascunho do agente.
- Para continuar uma conversa, envie o `session_id` de uma execução anterior do mesmo agente. Sessões com mais de `SESSION_RETENTION_DAYS` dias podem ter sido apagadas. Nesse caso, a execução falha e o cliente deve começar outra sem `session_id`.
- Limites: uma execução ativa por agente (`409 agent_busy`), `MAX_AGENTS_PER_INSTANCE` por instância (`429 instance_busy`) e prompt até 100 KB.
- Instância desligada: `409 instance_not_running` (ou liga sozinha se `AUTO_START=true`).
- Agente sem conexão válida: `409 agent_not_authenticated`, antes de criar a execução.

### Stream (SSE)

`GET /api/instances/{id}/runs/{runId}/stream`, com o cabeçalho `apikey`.

| Evento | `data` |
|---|---|
| `status` | `{ "run_id", "agent", "status": "running" }` |
| `session` | `{ "type": "session", "session_id" }` |
| `delta` | `{ "type": "delta", "text" }` (texto incremental da resposta) |
| `reasoning` | `{ "type": "reasoning", "text" }` (raciocínio incremental; Claude Code e Codex) |
| `tool` | `{ "type": "tool", "tool", "detail" }` (comando, arquivo ou ferramenta usada) |
| `done` | o `Run` final (`status`, `response`, `error_code`, `error_message`, `session_id`, `duration_ms`) |

- Cada evento tem `id:`. Para retomar após queda, reconecte com `Last-Event-ID` (ou `?after=`).
- O gateway envia `: ping` a cada 15 segundos.
- Até 2 minutos depois do fim, o stream reenvia todos os eventos. Depois disso, envia só `done`, montado a partir do histórico.

Repasse no backend: abra o stream do gateway com `fetch` no servidor e reescreva os eventos para o cliente pela conexão já autenticada do AC-WEB (SSE ou WebSocket). Nunca entregue a `stream_url` com a chave ao cliente.

Cancelar: `POST /api/instances/{id}/runs/{runId}/cancel`. O evento `done` chega com `status: cancelled`.

### Status finais de execução

| `status` | `error_code` possíveis |
|---|---|
| `succeeded` | — |
| `failed` | `agent_not_authenticated`, `agent_error`, `agent_exit`, `agent_incomplete`, `exec_failed` |
| `cancelled` | `cancelled_by_request`, `instance_stopped` |
| `timed_out` | `run_timeout` |
| `interrupted` | `gateway_restarted`, `stream_unavailable` |

## 8. Histórico

`GET /api/instances/{id}/history` devolve as 20 execuções mais recentes, da mais nova para a mais antiga. O limite é aplicado no banco: a 21ª execução apaga a mais antiga. O histórico sobrevive a reinícios.

## 9. Ciclo de vida e inatividade

- `POST …/start` e `POST …/stop` são idempotentes.
- Contam como atividade: `start`, execuções, logins, logout e operações de repositório. Leituras (`status`, `history`, listas) não mantêm a instância ligada, então o PWA pode consultar o estado sem impedir o desligamento.
- A instância desliga sozinha depois de `IDLE_TIMEOUT_SECONDS` sem atividade (padrão: 5 minutos). Execuções e logins em andamento nunca são interrompidos. A contagem recomeça quando terminam.
- Desligar não desconecta contas: logins dos CLIs, chaves, GitHub, repositórios e histórico continuam.
- `DELETE /api/instances/{id}?confirm={id}` apaga container, volume (logins de conta, repositórios e sessões), conexões (chaves e GitHub) e histórico. Peça confirmação explícita na interface.

## 10. Erros

Formato único:

```json
{ "error": { "code": "instance_not_running", "message": "Start the instance before this operation" } }
```

- `code` é estável. Traduza no cliente (PT, EN e ES) e use texto genérico para códigos desconhecidos.
- `message` é técnico e em inglês. Não o mostre ao usuário final.
- Status usados: `400` validação, `401` chave, `403` acesso negado pelo GitHub, `404` não encontrado, `409` estado incompatível, `429` limites, `502` falha do GitHub ou do Git, `503` GitHub não configurado, imagem ausente ou Docker indisponível.

## 11. Checklist do backend AC-WEB

- [ ] `AGENT_GATEWAY_URL` e `AGENT_GATEWAY_KEY` só no ambiente do servidor.
- [ ] Mapeamento `userId ↔ instanceId` e verificação de propriedade no `ac-services`.
- [ ] Rotas próprias do AC-WEB, por exemplo `/v1/agents/*`, com o mesmo controle de acesso das demais.
- [ ] Repasse do SSE sem expor URL nem chave do gateway.
- [ ] Página de retorno do GitHub (`GITHUB_REDIRECT_URL`) que chama `POST /api/instances/{id}/github/callback` para a instância do usuário da sessão.
- [ ] Tradução dos `error.code`, estados de instância, conexão e execução nos 3 idiomas.
- [ ] Confirmação explícita antes de excluir a instância.

## Limites de uso dos agentes

`GET /api/instances/{id}/agents/{agent}/usage` consulta o CLI dentro da instância ligada, sem enviar prompt nem gastar tokens, e devolve `{ "usage": <payload cru do provedor> | null }`:

| Agente | Origem do payload |
|---|---|
| `claude` | resposta do `control_request` `get_usage` (`rate_limits_available`, `rate_limits.five_hour`, `seven_day`…) |
| `codex` | resultado de `account/rateLimits/read` do `codex app-server` |
| `antigravity` | JSON de `agy -p /usage --output-format json` (`command.data.groups[].buckets[]`) |

Agente não conectado devolve `null` sem executar nada (o `agy` sem login espera um login interativo). Cada consulta roda com `timeout 20` dentro do container. O AC-WEB interpreta o payload com os mesmos parsers do Arara AI (`src/cli/usage.ts`), mostra os limites no seletor de modelo e calcula quanto cada resposta gastou comparando a leitura antes e depois da execução.

