# autosk daemon — общий UDS-сокет, мульти-проект

**Date:** 2026-05-18
**Status:** Spec locked (ask_user rounds 2026-05-18), ready for autonomous build.
**Predecessor:** [`20260517-Daemon-Plan.md`](20260517-Daemon-Plan.md),
[`20260517-Workflows-Plan.md`](20260517-Workflows-Plan.md).

---

## 1. Цель

Сделать так, чтобы **один экземпляр** `autosk daemon` на хост обслуживал
**произвольное количество проектов**. Текущий привязанный-к-`--cwd`
TCP-HTTP-демон заменяется на демона, слушающего общий unix-сокет; проект
выбирается клиентом на каждый запрос.

Иначе говоря: сегодня `autosk daemon serve` это «процесс на проект»; после
этой итерации это «процесс на машину», а проект становится свойством
запроса.

Все «удалённые» HTTP-сценарии (TCP-bind, токен, curl с порта, любые сетевые
интеграции) **удаляются полностью** на этом шаге и **позже будут
переделаны заново** под новые требования.

---

## 2. Зафиксированные решения (ask_user 2026-05-18)

| Тема | Решение |
|---|---|
| Транспорт | **HTTP-over-UDS**: оставить `net/http` для request/response, поменять listener с `net.Listen("tcp", ...)` на `net.Listen("unix", ...)`. |
| Сокет | `~/.autosk/daemon.sock` по умолчанию (директория `0700`, сокет `0600`). Переопределяется `--sock` / `AUTOSK_SOCK`. |
| Single-instance | Bind сокета: если живой (`net.Dial("unix", path)` ок) → fail; если stale (refused/ENOENT/EEXIST без живого пира) → `os.Remove` + bind. Без отдельного pidfile. |
| Контекст проекта | Клиент шлёт **`X-Autosk-Cwd`** (обязательный, абсолютный путь) и опционально **`X-Autosk-DB`** (абсолютный путь к `.autosk/db`). Демон сам резолвит через `projectdb.Resolve(cwd, dbOverride)`. |
| AUTOSK_DB env | Только клиентское окружение. Клиент читает свой `AUTOSK_DB`/`--db` и кладёт в `X-Autosk-DB`. Демон **игнорирует** свой собственный `AUTOSK_DB`. |
| Жизненный цикл проекта | Лениво: первый запрос с cwd X открывает БД, поднимает per-project executor/poller. Висит в памяти пока демон жив (без idle-выгрузки). |
| Concurrency | **Глобальный** worker pool: один `--workers N` на весь демон, единая FIFO-очередь по `(project, job_id)`. |
| Аутентификация | Только файловые права. `--token-file` и связанный auth middleware **удаляются**. |
| Скоуп list/health | По проекту клиента по умолчанию; флаг `--all-projects` (или query `?all=true`) даёт агрегированный вид по загруженным проектам. |
| SSE (`/v1/jobs/{id}/stream`) | Оставить работать поверх UDS — никаких изменений в логике, только перенос транспорта. |
| `daemon submit` (CLI + handler + типы) | Удалить целиком: server-side уже отвечал 501, всю работу тянет poller. |
| Внешние клиенты (extension, runtime) | Не трогаем — они общаются с autosk через CLI, не через демон. |

---

## 3. Что удаляется

### 3.1 Код
- `internal/daemon/server/server.go::handleSubmit` (вместе с `Server.routes` записью для POST `/v1/jobs`).
- `internal/daemon/api/types.go::SubmitRequest` + связанная `Validate()`.
- `internal/daemon/server/server.go::authMiddleware` (вместе с `Deps.Token` полем).
- `Deps.DefaultCwd` и `HealthResponse.DefaultCwd` — на демон‑уровне больше не существует «дефолтного cwd». Поле в HealthResponse, если оставляем поле `db_path`, переименовываем в per-project смысл (см. §5.6).
- В `cmd/autosk/daemon.go`:
  - флаги `--bind`, `--token-file`, `--cwd` у `daemon serve`;
  - функция `newDaemonSubmitCmd` и её регистрация;
  - флаги `--daemon-url`, `--daemon-token-file` у клиентских подкоманд.

### 3.2 Документация
- `docs/daemon.md`: убираются разделы про TCP, токены, curl, Bearer. Заменяются на «UDS + headers».
- `docs/plans/20260517-Daemon-Plan.md` помечается как superseded в части транспорта/auth/scope (ссылкой на этот документ); схема `daemon_runs` и executor-pipeline остаются.

### 3.3 Тесты
- `cmd/autosk/daemon_e2e_test.go` переезжает на UDS-клиент.
- `internal/daemon/server/server_test.go`, `sse_test.go` стартуют сервер на временном unix-сокете и шлют заголовки.
- Удаляются auth-тесты (`Token`/`401`).

---

## 4. Что добавляется

### 4.1 Новый пакет `internal/daemon/projectmgr`

Per-process кэш «загруженных» проектов. Один Manager на демон.

```go
package projectmgr

type Key string // canonical absolute project root (filepath.Clean + EvalSymlinks)

type Project struct {
    Root        string                  // canonical root (== Key)
    DBPath      string                  // absolute path to .autosk/db
    Tasks       store.Store             // *doltlite.DB
    Runs        *runstore.Store
    Agents      *agent.Store
    Workflows   *workflow.Store
    Comments    *comments.Store
    Signals     *step.Store
    Executor    *executor.Executor
    Poller      *poller.Poller
    // book-keeping
    OpenedAt    time.Time
    closeOnce   sync.Once
    closeFn     func() error
}

type Manager struct {
    mu        sync.Mutex
    projects  map[Key]*projectEntry
    deps      Deps          // shared sched, pkgregistry, cfg, logger
}

type Deps struct {
    Sched        *scheduler.Scheduler
    Packages     *pkgregistry.Registry
    ExecCfg      executor.Config        // PIBin, Grace, IdleTimeout (NO ProjectRoot)
    PollInterval time.Duration
    Logger       *slog.Logger
}

// Resolve / open a project. Idempotent. Cwd must be absolute.
// If dbOverride != "" it wins over walk-up.
func (m *Manager) Resolve(ctx context.Context, cwd, dbOverride string) (*Project, error)

// List currently-loaded projects (for --all-projects).
func (m *Manager) Loaded() []*Project

// CloseAll on daemon shutdown.
func (m *Manager) CloseAll(ctx context.Context) error
```

Семантика `Resolve`:
1. Валидируем: `cwd` непустой и абсолютный; иначе `ErrInvalidCwd`.
2. Используем `projectdb.Resolve(cwd, dbOverride)`. **AutoInit не вызываем** — демон не создаёт `.autosk/` без явного `autosk init`. Если `ErrNotFound` → возвращаем `ErrProjectNotFound`.
3. Канонизируем `filepath.Dir(filepath.Dir(dbPath))` через `filepath.EvalSymlinks` → это `Key`.
4. Под мьютексом смотрим `projects[Key]`. Если есть — отдаём.
5. Иначе создаём `projectEntry{readyCh:make(chan struct{})}`, помещаем в карту, отпускаем мьютекс, **открываем проект** (DB, migrate, stores, executor, poller, restart-recovery), закрываем `readyCh`. Параллельные клиенты, попавшие на ту же запись, ждут `readyCh` и получают тот же `*Project`. При ошибке открытия удаляем запись и возвращаем error всем ожидающим.

Открытие проекта:
- `doltlite.New().Open(ctx, dbPath)` → `Migrate(ctx)`.
- Конструируем `runstore`, `agent`, `workflow`, `comments`, `step` поверх одной `*sql.DB`.
- **Restart recovery**: одним SQL пробегом переписать `daemon_runs WHERE status='running'` → `status='failed', error='daemon_restart'`, `finished_at=now`. Делаем это до старта поллера.
- Конструируем `*executor.Executor` с `Config{ProjectRoot: Root, SessionDirRoot: <Root>/.autosk/sessions, ...унаследовано от ExecCfg}`.
- Конструируем `*poller.Poller(db, runs, sched, Config{Interval: cfg.PollInterval})`, **обёрнутый так, чтобы передавать project key в scheduler.Enqueue** (см. §4.2).
- Запускаем `poller.Start(ctx)`.

### 4.2 Scheduler — квалифицированные jobs

Сейчас `scheduler.Enqueue(jobID string)`. Переходим на:

```go
type Job struct {
    Project projectmgr.Key
    ID      string // job_id, уникальный только внутри проекта
}

func (j Job) String() string // "<project>::<jobID>" — для логов

type Executor interface {
    Run(ctx context.Context, job Job) error
}

scheduler.Enqueue(Job) error
scheduler.Cancel(Job) error
```

Внутри scheduler queue/active maps ключуем по `Job` (через канонический строковый ключ). Это позволяет:
- Одной FIFO-очереди обслуживать все проекты.
- `daemon cancel` корректно находить job в правильном проекте.
- Полно изолировать счётчики выполняющихся per-project (для `--all-projects` health).

Глобальный `Executor.Run(ctx, job)` живёт на уровне демона: смотрит `projects[job.Project]`, вызывает `proj.Executor.Run(ctx, job.ID)`. То есть `internal/daemon/executor` **не меняется** в части собственного API — меняется только обёртка в `cmd/autosk/daemon.go`.

### 4.3 HTTP transport — UDS

```go
listener, err := net.Listen("unix", sockPath)
// chmod 0600 после Listen
httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10*time.Second}
go httpSrv.Serve(listener)
```

Single-instance:
1. Гарантируем `os.MkdirAll(filepath.Dir(sockPath), 0o700)`.
2. `net.Dial("unix", sockPath)` с таймаутом `200ms`:
   - успех → fail с `daemon already running at <sockPath>`.
   - `ENOENT` → bind.
   - `ECONNREFUSED` / `ECONNRESET` → `os.Remove(sockPath)` + bind. Если remove упал и это не `ENOENT` → fail.
3. После `net.Listen` сразу `os.Chmod(sockPath, 0o600)`.
4. На shutdown — `httpSrv.Shutdown`, потом `os.Remove(sockPath)` (best-effort).

### 4.4 Middleware: проектный контекст

Новый middleware в `internal/daemon/server`:

```go
func (s *Server) projectMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // /v1/version, /v1/healthz?all=true — без проекта; всё остальное требует cwd.
        if exemptPath(r) {
            next.ServeHTTP(w, r)
            return
        }
        cwd := r.Header.Get("X-Autosk-Cwd")
        if cwd == "" || !filepath.IsAbs(cwd) {
            writeError(w, http.StatusBadRequest, "X-Autosk-Cwd header required (absolute path)", nil)
            return
        }
        db := r.Header.Get("X-Autosk-DB") // optional
        proj, err := s.deps.Projects.Resolve(r.Context(), cwd, db)
        if err != nil {
            switch {
            case errors.Is(err, projectmgr.ErrInvalidCwd):
                writeError(w, http.StatusBadRequest, err.Error(), nil)
            case errors.Is(err, projectmgr.ErrProjectNotFound):
                writeError(w, http.StatusNotFound, "no .autosk/db found from "+cwd, map[string]any{"cwd": cwd})
            default:
                writeError(w, http.StatusInternalServerError, "open_project: "+err.Error(), nil)
            }
            return
        }
        ctx := context.WithValue(r.Context(), projectKey{}, proj)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

func projectFromCtx(ctx context.Context) *projectmgr.Project // helper
```

Exempt paths:
- `GET /v1/version` — всегда без проекта.
- `GET /v1/healthz?all=true` — без проекта; `?all=false` (default) требует cwd как обычно.

### 4.5 Handlers — переезд на `projectFromCtx`

Каждый из `handleList`, `handleGet`, `handleCancel`, `handleMessages`, `handleStream`, `handleHealth (без ?all)` берёт `proj := projectFromCtx(r.Context())` и работает с `proj.Runs`, `proj.DBPath`, `proj.Root` вместо `s.deps.Runs`/`s.deps.DBPath`.

`handleCancel` дополнительно передаёт `scheduler.Job{Project: Key(proj.Root), ID: jobID}` в `sched.Cancel`.

### 4.6 Aggregated health (`?all=true`)

```json
{
  "ok": true,
  "workers": 4,
  "projects": [
    {
      "root":      "/Users/me/dev/foo",
      "db_path":   "/Users/me/dev/foo/.autosk/db",
      "queued":    1,
      "running":   2,
      "opened_at": "2026-05-18T10:00:00Z"
    },
    ...
  ]
}
```

`api.HealthResponse` расширяется опциональным `Projects []HealthProject`. Когда `?all` не задан — поле опущено, и в ответе сохраняется текущая форма (минус `default_cwd`), дополненная `project_root`.

### 4.7 Клиентская сторона (`cmd/autosk/daemon.go`)

`daemonClient` теперь:
- Конструирует `http.Client` с `Transport: &http.Transport{DialContext: func(...) { return net.Dial("unix", sockPath) }}`. Базовый URL — `http://autosk` (host игнорируется в UDS).
- В каждом запросе ставит `X-Autosk-Cwd` из `--cwd`/`os.Getwd()` (всегда после `filepath.Abs`).
- Если у клиента задан `AUTOSK_DB` или `--db` — ставит `X-Autosk-DB`.
- Никакого `Authorization` / `--token-file`.

Новые/удалённые флаги:

| Команда | Было | Стало |
|---|---|---|
| `daemon serve` | `--bind 127.0.0.1:7878`, `--token-file`, `--cwd`, `--workers`, `--grace`, `--idle-timeout`, `--poll-interval`, `--pi-bin`, `--session-dir` | `--sock ~/.autosk/daemon.sock` (env `AUTOSK_SOCK`), `--workers`, `--grace`, `--idle-timeout`, `--poll-interval`, `--pi-bin`, `--session-dir-root` (опц., применяется ко всем проектам как `<projectRoot>/.autosk/sessions` если пуст) |
| `daemon status/list/cancel/messages` | `--daemon-url`, `--daemon-token-file` | `--sock` (env `AUTOSK_SOCK`), глобальные `--cwd`/`--db` (или через корневые флаги, если уже есть). `daemon list` получает `--all-projects` (булевый), который добавляет `?all=true` к health/list. |
| `daemon submit` | — | **удаляется** |

Корневой CLI уже использует `flagDB` (см. `cmd/autosk/main.go`) — клиент будет использовать ту же резолюцию для `X-Autosk-DB`.

---

## 5. Структурные правки по файлам

### 5.1 Новые файлы
- `internal/daemon/projectmgr/projectmgr.go` — Manager, Project, Deps, ошибки.
- `internal/daemon/projectmgr/projectmgr_test.go` — параллельный open идемпотентен; ENOENT cwd → ErrProjectNotFound; symlinks канонизируются; restart-recovery срабатывает один раз.
- `internal/daemon/server/middleware.go` — `projectMiddleware`, `projectFromCtx`, exempt-список.
- `internal/daemon/uds/uds.go` (мелкий helper) — `Listen(path string) (net.Listener, error)` с single-instance‑семантикой + `cleanup(path)`.
- `cmd/autosk/daemon_uds_e2e_test.go` — e2e: поднимаем демона на временном сокете, регистрируем два проекта (две `tmp/.autosk/`), отправляем запросы из обоих, проверяем изоляцию runs и работу `--all-projects`.

### 5.2 Изменяемые файлы
- `cmd/autosk/daemon.go` — replace `daemonServeCmd` тело; replace `daemonClient`; remove `newDaemonSubmitCmd`; refactor флаги. Глобальный scheduler конструируется один; manager создаётся один; передаются в `server.New`.
- `internal/daemon/server/server.go` — drop `handleSubmit`, `authMiddleware`, `Deps.Token`, `Deps.DefaultCwd`, `Deps.DBPath`. Добавить `Deps.Projects *projectmgr.Manager`, `Deps.Sched *scheduler.Scheduler`. Все handlers переписать на `projectFromCtx`. Маршруты остаются те же минус POST `/v1/jobs`.
- `internal/daemon/server/sse.go` — заменить `s.deps.Runs.GetRun` → `projectFromCtx(r.Context()).Runs.GetRun`; пути транскрипта тоже из proj.
- `internal/daemon/api/types.go` — удалить `SubmitRequest` и его `Validate()`; в `HealthResponse` убрать `DefaultCwd`, добавить опциональное `ProjectRoot` + `Projects []HealthProject` (новый тип).
- `internal/daemon/scheduler/scheduler.go` + `scheduler_test.go` — ввести `Job` тип, обновить `Enqueue`/`Cancel`/`Executor` сигнатуры; внутренние мапы по `Job.String()`.
- `internal/daemon/poller/poller.go` — `enqueueCandidate` строит `scheduler.Job{Project: projectKey, ID: jobID}`. Конструктор принимает `Project Key` явным параметром или поллер создаётся внутри projectmgr и project знает свой Key.
- `internal/daemon/server/server_test.go`, `sse_test.go` — поднимать сервер через unix socket, использовать `httptest.Server` нельзя в чистом виде → построить руками `&http.Server{Handler: ...}` поверх временного `net.Listen("unix")`; клиент с custom transport. Все запросы — с `X-Autosk-Cwd` заголовком.
- `cmd/autosk/daemon_e2e_test.go` — переезд на UDS.
- `docs/daemon.md` — переписать transport/auth/quickstart разделы.
- `AGENTS.md` — секцию «When a daemon is running» обновить под UDS-семантику и явное упоминание headers/--sock.

### 5.3 Удаляемые/обнуляемые
- Все упоминания `7878`, `--bind`, `--daemon-url`, `--token-file`, `--daemon-token-file` в коде и доке.
- `cmd/autosk/daemon.go::newDaemonSubmitCmd`, `printJobDetail` остаётся.
- `internal/daemon/api/types.go::SubmitRequest` + `ErrInvalidRequest` + `Validate`.

---

## 6. Лайфсайкл демона

### 6.1 Старт
1. Распарсить флаги; resolve sockPath.
2. Открыть `pkgregistry` (как сейчас).
3. Сконструировать глобальный `scheduler.Scheduler` (workers, executor = lookup-and-dispatch).
4. Сконструировать `projectmgr.Manager` с deps (sched, pkgregistry, exec/poll cfg).
5. `uds.Listen(sockPath)` (с single-instance проверкой).
6. `chmod 0600`.
7. `http.Server.Serve(listener)` в горутине.
8. Перехват `SIGINT`/`SIGTERM`.

### 6.2 Shutdown (по SIGINT/SIGTERM)

> **Note (post-review, 2026-05-18):** ранний черновик этого раздела
> ставил `manager.CloseAll` перед `scheduler.Stop`. Это закрывало
> per-project `*sql.DB` раньше, чем активные воркеры успевали
> зафиксировать терминальный `MarkCancelled`/`MarkFailed`, и тихо
> деградировало graceful-shutdown до восстановления из крэша на
> следующем старте. Реализация теперь разделяет шаги так, чтобы
> ни один `doltlite.Close` не выполнялся, пока хотя бы один
> воркер ещё пишет в свой run.

1. `httpSrv.Shutdown(ctx, 15s)` — перестать принимать новые запросы.
2. `manager.StopPollers(ctx)` — остановить per-project поллеры, чтобы новые `daemon_runs` не появлялись.
3. `scheduler.Stop(ctx)` — отменить и дождаться активных воркеров; они успевают записать `MarkCancelled`/`MarkFailed` в ещё открытые `*sql.DB`.
4. `manager.CloseDBs(ctx)` — только теперь безопасно закрывать per-project doltlite-store'ы.
5. `uds.Cleanup(sockPath)` (`os.Remove`) — best-effort.

`manager.CloseAll(ctx)` остаётся как удобный комбинированный
хелпер для тестов и для не-scheduler-овых сценариев: внутри он
вызывает `StopPollers` затем `CloseDBs`. Продакшен-путь
в `cmd/autosk/daemon.go` вызывает эти два метода явно, вклинив
`sched.Stop` между ними; `CloseAll` сам по себе scheduler
не трогает (это вне его обязанностей).

### 6.3 Запрос
1. Принимаем HTTP запрос на UDS.
2. Если путь не exempt → `projectMiddleware` извлекает `X-Autosk-Cwd`, резолвит проект, кладёт в context. Если проект не найден / cwd плохой → 4xx, дальше не идём.
3. Handler работает в контексте конкретного проекта.

### 6.4 Поллер для проекта
- Запускается один раз при открытии проекта.
- Тикает `cfg.PollInterval`. Каждый тик: select поверх своей `*sql.DB`, для каждого кандидата создаёт `daemon_runs` row и шлёт `sched.Enqueue(Job{Project: key, ID: jobID})`.
- Никаких изменений в SQL-запросе.

### 6.5 Глобальный scheduler.Executor
```go
sched := scheduler.New(nil /* runs больше не централизован */, scheduler.ExecutorFunc(func(ctx context.Context, job scheduler.Job) error {
    proj, err := mgr.Get(job.Project) // быстрый lookup, без resolve
    if err != nil { return err }
    return proj.Executor.Run(ctx, job.ID)
}), scheduler.Config{Workers: workers})
```

`scheduler.Runs` поле уходит (текущий `runstore` использовался только для рекавери на старте — теперь это per-project делает projectmgr).

---

## 7. Обратная совместимость и миграции

- **Внешний контракт ломаем**: HTTP TCP, токены, `daemon submit` — всё снято. Это сознательное решение пользователя.
- **Схема `.autosk/db` не меняется**. Существующие БД продолжают работать; добавляется только restart-recovery sweep при первом открытии.
- **Старые скрипты** с `curl localhost:7878/...` ломаются и должны быть переписаны на `autosk daemon ...` через UDS или подождать возвращения HTTP-режима в следующей итерации. Это явно отмечается в `docs/daemon.md`.

---

## 8. План фаз для автономной работы

Каждая фаза — отдельная autosk задача с явными зависимостями (`autosk block`). Внутри фазы — самодостаточные изменения с тестами.

| # | Задача | Зависит от |
|---|---|---|
| D1 | `internal/daemon/projectmgr`: пакет, ошибки, lazy-open, restart-recovery, тесты. | — |
| D2 | `internal/daemon/scheduler`: `Job` тип, рефактор Enqueue/Cancel/Executor, обновлённые тесты. | — |
| D3 | `internal/daemon/poller`: принимать Project Key, использовать новый Scheduler.Enqueue. | D2 |
| D4 | `internal/daemon/uds`: helper для single-instance UDS listen + unlink stale. | — |
| D5 | `internal/daemon/server`: drop submit/auth/DefaultCwd; project middleware; handlers переписать на `projectFromCtx`; SSE-handler такой же путь; aggregated health. | D1, D2 |
| D6 | `internal/daemon/api/types.go`: удалить SubmitRequest/Validate; HealthResponse расширить ProjectRoot/Projects. | D5 |
| D7 | `cmd/autosk/daemon.go::daemonServeCmd`: новые флаги, projectmgr, UDS listener, lifecycle. Убрать subcmds/флаги, удалить `newDaemonSubmitCmd`. | D1, D2, D3, D4, D5 |
| D8 | `cmd/autosk/daemon.go::daemonClient` + клиентские подкоманды: UDS transport, заголовки `X-Autosk-*`, флаг `--sock`, `--all-projects`. | D7 |
| D9 | E2E-тест `cmd/autosk/daemon_uds_e2e_test.go`: два проекта, общий сокет, изоляция runs, `--all-projects`. | D7, D8 |
| D10 | Документация: `docs/daemon.md` переписать, обновить `AGENTS.md`, отметить `20260517-Daemon-Plan.md` как частично superseded. | D7, D8 |
| D11 | Очистка хвостов: убрать упоминания 7878/--bind/--token-file/--daemon-url в коде и доке; `go vet`/`gofmt`/`go test ./...` зелёные. | D7, D8, D9, D10 |

---

## 9. Acceptance checklist

- [ ] `autosk daemon serve` стартует без флага `--cwd`, биндится только на `~/.autosk/daemon.sock` (или `--sock`/`$AUTOSK_SOCK`); права `0600`, директория `0700`.
- [ ] Второй параллельный `autosk daemon serve` падает с `daemon already running at <sock>`.
- [ ] Если сокет «протух» (нет процесса) — следующий старт его удаляет и поднимается.
- [ ] `autosk daemon list` из проекта A показывает только jobs A; из проекта B — только B. `--all-projects` показывает оба.
- [ ] Submit-CLI и POST `/v1/jobs` отсутствуют (CLI: команда не зарегистрирована; HTTP: маршрут 404 от ServeMux).
- [ ] `GET /v1/healthz` без `?all` требует `X-Autosk-Cwd`; с `?all=true` — нет.
- [ ] Любой запрос без `X-Autosk-Cwd` (кроме exempt) → 400 `X-Autosk-Cwd header required`.
- [ ] Запрос с `X-Autosk-Cwd`, указывающим в директорию без `.autosk/db`, → 404 `no .autosk/db found from <cwd>`.
- [ ] `X-Autosk-DB` (если задан) переопределяет walk-up; клиент кладёт его, когда у пользователя задан `AUTOSK_DB` или `--db`.
- [ ] Демон **не** читает свой `AUTOSK_DB`.
- [ ] При перезапуске демона `daemon_runs.status='running'` в каждом первый-раз-открываемом проекте переводятся в `failed/'daemon_restart'`.
- [ ] SSE `/v1/jobs/{id}/stream` продолжает работать через UDS (verified via test + curl `--unix-socket`).
- [ ] `--token-file`/`--daemon-token-file`/`--bind`/`--daemon-url`/`--cwd` (на daemon-командах) отсутствуют.
- [ ] `go test ./...` зелёный; `go vet ./...` зелёный.
- [ ] `docs/daemon.md`, `AGENTS.md`, `docs/plans/20260517-Daemon-Plan.md` обновлены / помечены.

---

## 10. Out of scope (на следующую итерацию)

- Возвращение «удалённого» HTTP-API (TCP-bind, авторизация, потенциально mTLS) — будет переделано заново по новым требованиям.
- Idle-выгрузка простаивающих проектов из памяти.
- Per-project лимит worker'ов / приоритеты между проектами.
- Multi-user / shared-host сценарии (SO_PEERCRED, namespace-isolation).
- `autosk daemon attach`/`detach` для явной регистрации проектов под polling.
- Re-attach к выжившим pi-процессам после рестарта демона.
- Worktree-per-job изоляция.
