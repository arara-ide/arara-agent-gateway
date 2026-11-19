# Contrato de integração com o AC-WEB

Este documento descreve como o AC-WEB PWA e o AC-WEB Desktop usam o Arara Agent Gateway. Os dois usam o mesmo contrato, sempre por meio do backend do AC-WEB. A referência formal de cada rota está em [`openapi.yaml`](openapi.yaml), também servida em `GET /api/openapi.yaml`.

## 1. Quem fala com quem

```text
PWA / Desktop ──(sessão do AC-WEB)──> backend AC-WEB ──(HTTPS + apikey + X-User-Id)──> Gateway
```

- O navegador e o app Desktop **nunca** chamam o gateway direto e **nunca** recebem `API_KEY` nem `INTEGRATION_API_KEY`.
- O backend do AC-WEB autentica o usuário final como já faz hoje, pega o ID dele da própria sessão e chama o gateway com esse ID.
- Nada do que o cliente envia em corpo, query ou cabeçalho pode virar `X-User-Id`. O ID sempre vem da sessão validada no servidor.

## 2. Autenticação

| Cabeçalho | Valor |
|---|---|
| `apikey` | `INTEGRATION_API_KEY` (recomendado) ou `API_KEY` |
| `X-User-Id` | ID estável do usuário no AC-WEB, `^[A-Za-z0-9._:@-]{1,128}$` |

- Com a `INTEGRATION_API_KEY`, o `X-User-Id` é obrigatório (`400 user_scope_required`) e toda operação fica restrita aos recursos desse usuário.
- Instância, execução ou stream de outro usuário respondem `404 not_found`. O gateway não diz se o recurso existe.
- Rotas `/api/users/{userId}/...` exigem `userId` igual ao `X-User-Id`. Se forem diferentes, a resposta é `404`.
- A `API_KEY` administrativa é da dashboard e da operação. Ela não deve ficar no backend do AC-WEB, a menos que seja a única chave configurada.
- Em produção, publique o gateway só em HTTPS (proxy reverso) ou em rede privada entre os servidores.

Exemplo de cliente no backend (TypeScript, NestJS ou qualquer Node):

```ts
const GATEWAY_URL = process.env.AGENT_GATEWAY_URL!;
const GATEWAY_KEY = process.env.AGENT_GATEWAY_KEY!;

async function gateway(userId: string, method: string, path: string, body?: unknown) {
  const res = await fetch(`${GATEWAY_URL}/api${path}`, {
    method,
    headers: {
      apikey: GATEWAY_KEY,
      'X-User-Id': userId,
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

## 3. Associação usuário ↔ instância

- Cada usuário tem uma instância principal. `POST /api/instances` é idempotente: devolve `201` com `created: true` na primeira vez e `200` com a mesma instância depois.
- O backend não precisa guardar o `instance_id`. Use `GET /api/users/{userId}/instance` para descobrir a instância do usuário da sessão.
- Se guardar o `instance_id`, o gateway confere o dono em toda chamada com `X-User-Id`. Conhecer o ID de outra instância não dá acesso a ela.

Fluxo recomendado ao abrir o módulo de agentes:

1. `POST /api/instances` com `{}` (cria ou devolve a instância do usuário).
2. `GET /api/instances/{id}/status` para mostrar o estado da instância, dos três agentes e do GitHub.
3. Se `instance.state` for `stopped`, mostre o botão "Ligar", que chama `POST /api/instances/{id}/start`.

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
| `connected` | Pronto para uso. `account` mostra e-mail, login GitHub ou a chave mascarada (`AIza…0000`). |
| `disconnected` | O usuário desconectou. |
| `expired` | Login de conta que deixou de valer (token revogado ou vencido). Peça novo login. |
| `error` | Chave de API recusada pelo provedor. Peça uma chave nova. |
| `setup_required` | Nunca configurado. |

`method` vale `account` (login oficial do CLI), `api_key` (chave do próprio usuário) ou `oauth` (GitHub).

## 5. Conexão dos agentes

| Agente | `account` | `api_key` | Observação |
|---|---|---|---|
| `claude` | Sim: `claude auth login`. A pessoa abre a URL e cola o código. | `ANTHROPIC_API_KEY` | A conta fica nos arquivos do CLI dentro do volume da instância. |
| `codex` | Sim: `codex login --device-auth`. A pessoa abre a URL e digita o código. | `CODEX_API_KEY` | O login por código de dispositivo precisa estar habilitado na conta ChatGPT. |
| `antigravity` | Não | `GEMINI_API_KEY` com `modelProvider: gemini` | O login Google do Antigravity exige keyring local, e os termos proíbem acesso por ferramentas de terceiros. |

### Chave de API (vale para os três)

```http
PUT /api/users/{userId}/agents/{agent}/api-key
{ "api_key": "..." }
```

A chave fica cifrada no gateway e só entra como variável de ambiente durante cada execução. Ela nunca é devolvida. Para remover, use `DELETE` na mesma rota.

### Login de conta (Claude Code e Codex)

A instância precisa estar ligada.

1. `POST /api/instances/{id}/agents/{agent}/login` devolve `login`:
   ```json
   { "id": "login_…", "status": "waiting_for_user", "url": "https://…", "code": "ABCD-12345", "needs_input": false, "expires_at": "…" }
   ```
2. Mostre `url` como link para o usuário abrir no navegador dele. Se houver `code`, mostre-o (Codex).
3. Se `needs_input` for `true` (Claude), mostre um campo para o código exibido depois do login e envie com `POST …/login/input` `{ "code": "…" }`. O campo aceita uma linha só, sem espaços. Nada além disso chega ao terminal.
4. Consulte `GET …/login` a cada 3 segundos até `status` virar `succeeded`, `failed` ou `cancelled`. Em `failed`, `error` vale `login_timeout`, `login_failed` ou `not_authenticated`.
5. Para cancelar, use `DELETE …/login`. Para sair da conta, use `POST …/logout`.

A sessão de login dura até 10 minutos e mantém a instância ligada enquanto estiver aberta.

## 6. GitHub

O GitHub funciona por usuário, com GitHub App (recomendado, token de usuário com validade curta e refresh) ou com OAuth App. O gateway usa `state` de uso único (10 minutos), vinculado ao usuário, e PKCE S256.

`GITHUB_REDIRECT_URL` aponta para uma página **do AC-WEB**, não do gateway. Assim, a conclusão acontece dentro da sessão autenticada do usuário, e um link de autorização criado por outra pessoa não consegue vincular a conta errada.

1. Usuário clica em "Conectar GitHub" no PWA/Desktop.
2. O backend chama `POST /api/users/{userId}/github/connect`, recebe `authorize_url` e redireciona o navegador para lá.
3. O GitHub redireciona para `GITHUB_REDIRECT_URL?code=…&state=…` no AC-WEB.
4. O backend do AC-WEB pega `code` e `state` da query e o usuário da sessão, e chama `POST /api/users/{userId}/github/callback` com `{ "code": "…", "state": "…" }`.
5. A resposta traz `connection.status = connected` e `account` (login GitHub). `400 github_invalid_state` indica link expirado, já usado ou de outro usuário.

Outras operações:

| Ação | Rota |
|---|---|
| Estado (valida o token no GitHub) | `GET /api/users/{userId}/github` |
| Listar repositórios acessíveis | `GET /api/users/{userId}/github/repos?page=1` |
| Clonar ou atualizar na instância | `POST /api/instances/{id}/repos` `{ "full_name": "owner/name", "select": true }` |
| Repositórios clonados | `GET /api/instances/{id}/repos` |
| Escolher o repositório padrão | `PATCH /api/instances/{id}` `{ "selected_repo": "owner/name" }` |
| Enviar o trabalho de um agente | `POST /api/instances/{id}/repos/{owner}/{name}/push` `{ "agent": "codex" }` |
| Desconectar (revoga no GitHub) | `DELETE /api/users/{userId}/github` |

- Cada agente trabalha num worktree próprio, na branch `arara/<agente>`. O push só aceita branches `arara/*` e nunca envia para a branch padrão.
- O token GitHub não fica no volume. O gateway o injeta como cabeçalho temporário em cada `clone`, `fetch` e `push`. Os agentes fazem commits locais e o push passa sempre pelo gateway.
- `409 github_expired` indica que o usuário precisa reconectar.

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

`GET /api/instances/{id}/runs/{runId}/stream`, com os mesmos cabeçalhos `apikey` e `X-User-Id`.

| Evento | `data` |
|---|---|
| `status` | `{ "run_id", "agent", "status": "running" }` |
| `session` | `{ "type": "session", "session_id" }` |
| `delta` | `{ "type": "delta", "text" }` (texto incremental da resposta) |
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
- A instância desliga sozinha depois de `IDLE_TIMEOUT_SECONDS` sem atividade (padrão: 5 minutos). Execuções e logins em andamento nunca são interrompidos. A contagem recomeça quando terminam.
- Desligar não desconecta contas: logins dos CLIs, chaves, GitHub, repositórios e histórico continuam.
- `DELETE /api/instances/{id}?confirm={id}` apaga container, volume (logins de conta, repositórios e sessões) e histórico. As chaves de API e o GitHub continuam na conta do usuário. Peça confirmação explícita na interface.

## 10. Erros

Formato único:

```json
{ "error": { "code": "instance_not_running", "message": "Start the instance before this operation" } }
```

- `code` é estável. Traduza no cliente (PT, EN e ES) e use texto genérico para códigos desconhecidos.
- `message` é técnico e em inglês. Não o mostre ao usuário final.
- Status usados: `400` validação, `401` chave, `403` acesso negado pelo GitHub ou usuário divergente, `404` não encontrado ou de outro usuário, `409` estado incompatível, `429` limites, `502` falha do GitHub ou do Git, `503` GitHub não configurado, imagem ausente ou Docker indisponível.

## 11. Checklist do backend AC-WEB

- [ ] `AGENT_GATEWAY_URL` e `AGENT_GATEWAY_KEY` só no ambiente do servidor.
- [ ] `X-User-Id` sempre vindo da sessão autenticada.
- [ ] Rotas próprias do AC-WEB, por exemplo `/v1/agents/*`, com o mesmo controle de acesso das demais.
- [ ] Repasse do SSE sem expor URL nem chave do gateway.
- [ ] Página de retorno do GitHub (`GITHUB_REDIRECT_URL`) que chama `…/github/callback` com o usuário da sessão.
- [ ] Tradução dos `error.code`, estados de instância, conexão e execução nos 3 idiomas.
- [ ] Confirmação explícita antes de excluir a instância.
