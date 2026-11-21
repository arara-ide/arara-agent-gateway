# AGENTS.md — Arara Agent Gateway

Instruções para qualquer agente de IA (e pessoa) que altere este repositório. `CLAUDE.md` aponta para este arquivo.

## 1. Identidade

- Nome: **Arara Agent Gateway**. Repositório público `arara-ide/arara-agent-gateway`, licença MIT.
- O que é: uma "Evolution API para agentes de programação". Um servidor central gerencia N instâncias independentes sob uma única `API_KEY` global; cada instância é um container Docker com Claude Code, Codex e Antigravity disponíveis, um volume persistente próprio e conexões próprias.
- Consumidor principal: backend do AC-WEB (`ac-services`), que organiza qual instância pertence a qual usuário. O gateway é independente e pode servir outros projetos.

## 2. Objetivos e escopo

- Gerenciar instâncias sob demanda (criar, ligar, desligar, excluir), executar prompts nos três CLIs com streaming, guardar histórico curto (20 execuções por instância) e desligar instâncias ociosas.
- Manter conexões por instância: GitHub (OAuth/GitHub App user token) e credenciais dos CLIs (login de conta e chave de API).
- Fora do escopo: cadastro/login/mapeamento de usuários finais, interface de chat para clientes, colaboração automática entre agentes, workflows, filas, cobrança, analytics. Não implemente recurso que não foi pedido.

## 3. Arquitetura

```text
AC-WEB (PWA/Desktop) → backend AC-WEB → HTTPS + apikey → Gateway (Go, um binário)
Gateway → Docker Engine (socket local) → 1 container por instância (imagem runtime compartilhada)
Gateway → SQLite (DATA_DIR/gateway.db)
```

- Um processo Go: API REST, dashboard estática embutida (`web/`), gerenciador de instâncias, orquestrador de execuções, gerenciador de inatividade e integração GitHub.
- Docker Engine é acessado pela API HTTP no socket Unix, sem SDK.
- Container de instância: usuário sem privilégio (`agent`, uid 1000), `cap_drop ALL`, `no-new-privileges`, rootfs somente leitura, `/tmp` em tmpfs, limites de memória/CPU/PIDs, rede bridge própria sem comunicação entre containers. Nunca `privileged`, nunca socket do Docker montado.
- Volume `aag-<instanceId>` montado em `/home/agent`: logins dos CLIs, sessões e repositórios. Parar o container não apaga o volume.
- Processos dos CLIs só existem durante uma execução ou login (`docker exec`). Nada fica rodando em segundo plano.

## 4. Integração com AC-WEB e responsabilidades

- Gateway: instâncias, execução dos CLIs, isolamento, persistência, GitHub por instância, credenciais, limites, streaming, API e dashboard admin.
- AC-WEB (`ac-services` + `ac-web`): login dos usuários finais, associação usuário ↔ instância, permissões, chat, PWA/Desktop.
- A `API_KEY` só existe em servidores. Nunca no navegador, no app Desktop, em `localStorage` ou em URL.
- Contrato completo em `docs/ac-web-integration.md` e `docs/openapi.yaml`. Mudou rota, campo ou erro: atualize os dois na mesma entrega.

## 5. Desempenho

- Go stdlib primeiro. Dependências de runtime atuais: `modernc.org/sqlite`. Nova dependência só com justificativa no PR.
- Proibido: Redis, PostgreSQL, filas, Kubernetes, Node no gateway, frameworks de frontend, workers permanentes extras, um container por CLI.
- Um único ticker de inatividade (`IDLE_CHECK_INTERVAL_SECONDS`). Nenhum polling por execução.
- Saídas de execução têm limite de tamanho; o histórico guarda no máximo 20 registros por instância, aplicado no banco.

## 6. Segurança

- Comparação da `API_KEY` em tempo constante. Sem chave válida: `401`.
- Segredos persistidos (tokens GitHub, API keys dos provedores) são cifrados com AES-256-GCM usando `CREDENTIALS_ENCRYPTION_KEY`, com o `instance_id` como dado associado. Nunca são devolvidos pela API nem exibidos na dashboard.
- Tokens GitHub nunca são gravados no volume da instância; o gateway injeta um cabeçalho HTTP temporário em cada `git clone/fetch/push`.
- Logins de conta dos CLIs ficam nos arquivos oficiais dos próprios CLIs, dentro do volume da instância. Documente qualquer mudança desse modelo.
- Entrada do usuário nunca é interpolada em shell: comandos vão para o Docker como lista de argumentos.
- Não publique `.env`, bancos, volumes, sessões, tokens ou arquivos de credencial. Não registre prompts, respostas ou tokens em log.

## 7. Padrões de código

- Go formatado com `gofmt`, `go vet` limpo. Identificadores, mensagens de log, erros da API e commits em inglês. Documentação em português.
- Sem comentários no código (exceto diretivas técnicas como `//go:embed` e `//go:build`). Explicações vão em `docs/decisions.md`.
- Lógica pura separada de adaptadores (Docker, HTTP, SQLite). Interfaces pequenas para permitir dublês nos testes.
- Sem duplicação de regra ou constante: extraia para um pacote compartilhado.
- Textos visíveis da dashboard vêm de `web/locales/{en,pt,es}.json` (mesmas chaves nos três). Datas e números via `Intl` com o idioma ativo.

## 8. Estrutura

```text
cmd/gateway/          entrada do binário
internal/config/      leitura e validação do ambiente
internal/store/       SQLite: esquema, instâncias, execuções, conexões, estados OAuth
internal/secret/      cifra autenticada de credenciais
internal/docker/      cliente mínimo da API do Docker Engine
internal/instance/    ciclo de vida das instâncias e travas por instância
internal/agent/       adapters dos CLIs (argumentos, parser de eventos, login)
internal/connect/     credenciais dos agentes (chave de API cifrada, status, login sem terminal)
internal/run/         execuções, limites, cancelamento, streaming e histórico
internal/idle/        desligamento por inatividade
internal/github/      OAuth, repositórios, operações Git
internal/api/         rotas, autenticação, erros, SSE
web/                  dashboard estática embutida
runtime/              Dockerfile da imagem compartilhada e scripts aag-run, aag-kill, aag-prune
docs/                 OpenAPI, integração AC-WEB, decisões
scripts/              testes reais com Docker e build da imagem de runtime (`build-runtime.sh`)
assets.go             embute web/ e docs/openapi.yaml no binário
```

## 9. API

- Prefixo `/api`. JSON em tudo, exceto o stream (`text/event-stream`).
- Autenticação: cabeçalho `apikey`.
- Erro padrão: `{"error":{"code":"snake_case","message":"English text"}}` com status HTTP coerente. Códigos são estáveis; clientes traduzem.
- IDs com prefixo: `inst_`, `run_`, `login_`. Datas em RFC 3339 UTC.
- Ciclo de vida idempotente: `start` em instância ligada e `stop` em instância parada retornam `200` com o estado atual.

## 10. Persistência

- SQLite em `DATA_DIR/gateway.db`, modo WAL, uma conexão de escrita. Migrações em `internal/store/schema.go`, só aditivas.
- Ao iniciar, o gateway reconcilia: execuções `running` sem processo viram `interrupted`, o estado dos containers é lido do Docker e processos órfãos dentro de containers ligados são encerrados.
- Excluir instância exige `confirm` igual ao ID e remove container, volume, conexões, execuções e estado da instância.

## 11. Git e branches

- `main`: estável, só recebe código testado vindo da `development`. `development`: trabalho diário.
- Conventional Commits em inglês, sem `Co-authored-by` ou assinatura de IA. Sem `--force`, `reset --hard` ou `--no-verify` sem autorização.
- CI (`.github/workflows/ci.yml`) roda em `main` e `development`: `gofmt`, `go vet`, `go test -race`, build do binário e das duas imagens.

## 12. Testes obrigatórios

- Toda mudança de comportamento traz teste unitário na mesma entrega. Sem rede, sem conta real, sem créditos de IA: use os dublês de `internal/docker` e as fixtures dos adapters.
- Comandos: `gofmt -l .` vazio, `go vet ./...`, `go test -race ./...`, `go build ./cmd/gateway`.
- Efeito real (Docker, containers, login, inatividade): `scripts/e2e-docker.sh`, com diretório temporário, porta e nomes isolados. No CI roda com `E2E_SKIP_PROVIDERS=1` (sem chamadas a provedores). Execução real com IA só na máquina do mantenedor, com modelos econômicos.
- Mudou o wrapper `runtime/bin/*`: reconstrua a imagem e rode o e2e completo, porque os dublês removem o wrapper.
- Proibido apagar, pular ou afrouxar teste.

## 13. Critérios de aceite de uma entrega

1. Testes acima passando, com evidência no PR.
2. `docs/openapi.yaml`, `docs/ac-web-integration.md` e `README.md` atualizados quando o contrato muda.
3. Nenhum segredo, sessão ou volume no diff.
4. Isolamento entre instâncias coberto por teste quando a rota ou serviço for novo.
5. Nenhuma dependência ou serviço novo sem justificativa.
