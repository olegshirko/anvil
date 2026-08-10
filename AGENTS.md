# AGENTS.md

Этот файл — краткая инструкция для AI-агентов, работающих с репозиторием Anvil.
Детальное описание архитектуры и причин ключевых решений — в `ARCHITECTURE.md`
(обязательно к прочтению перед нетривиальными изменениями).

## Обзор проекта

Anvil — минимальная альтернатива Lima / Docker Desktop / OrbStack для запуска
Docker-контейнеров на macOS (только Apple Silicon). Система состоит из двух
компонентов:

- **`vz-runner`** (Swift, `Sources/vz-runner/`) — единственный долгоживущий
  host-процесс. Управляет Linux VM через `Virtualization.framework`
  (snapshot/resume, virtiofs, NAT-сеть), открывает unix-сокеты
  `~/.anvil-vz/control.sock` (control plane), `~/.anvil-vz/docker.sock`
  (прокси Docker API) и `~/.anvil-vz/buildkit.sock` (прокси buildkit API
  для buildx remote driver), форвардит порты контейнеров на `localhost`.
- **`guest-agent`** (Go, `guest-agent/`) — PID 1 внутри VM. Слушает vsock
  (порт 1024 — length-prefixed JSON control channel, порт 1025 — эмуляция
  Docker API для `docker`/`docker compose`, порт 1026 — мост к unix-сокету
  buildkitd для buildx remote driver), сканирует containerd и пушит
  port mappings в vz-runner, запускает healthcheck'и, генерирует CNI-конфиги.
  Внутри VM работают containerd + nerdctl + runc + CNI plugins; systemd и SSH
  нет. buildkitd стартует лениво — при первом подключении к порту 1026 или
  первом `nerdctl build`.

Пользователь работает обычным Docker CLI: `docker context use anvil` указывает
на `~/.anvil-vz/docker.sock`, дальше HTTP-трафик проксируется в guest-agent
через vsock.

## Технологический стек и требования

- macOS на Apple Silicon, Xcode/Swift toolchain, `swift-tools-version:5.9`,
  `platforms: [.macOS(.v14)]` (`Package.swift`).
- Бинарник требует entitlement `com.apple.security.virtualization`
  (`entitlements.plist`) — обязательна ad-hoc подпись (`make sign`).
- Go-модуль `guest-agent` (`guest-agent/go.mod`, go 1.26); сборка кроссом:
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`.
- Python 3 для вспомогательных скриптов (`scripts/*.py`), без зависимостей.
- Внешние инструменты сборки: `limactl` (сборка initramfs внутри Lima VM
  `anvil`; fallback — локальный Docker), Docker CLI, curl.
- Скачанные артефакты (kernel, initramfs, container-tools) живут в
  `.download/` и не коммитятся.

## Сборка и основные команды

Всё управляется через `Makefile`:

- `make sign` — собрать release-бинарник и подписать с entitlements
  (генерирует `Sources/vz-runner/version.swift`, не редактировать вручную).
- `make guest-agent` — собрать guest-agent (linux/arm64) в
  `.download/alpine/guest-agent`.
- `make initramfs-containerd` — собрать initramfs с containerd/nerdctl/CNI
  (скрипт `scripts/build_initramfs_containerd.sh`, собирается внутри Linux-
  контейнера — в Lima VM `anvil`, либо через локальный Docker). buildkit-
  бинарники пакуются UPX: в CI ставится из apk, для офлайн-сборки в Lima
  используется статический бинарник из `.download/upx` (target
  `download-upx`, входит в зависимости).
- `make rebuild-all` — бинарник + guest-agent + initramfs одной командой.
- `make service-start` / `service-stop` / `service-restart` / `service-status` —
  управление демоном через `scripts/anvil-service.sh` (сохраняет и
  восстанавливает docker context).
- `make service-debug` — stop + удаление снапшота + cold boot с `DEBUG=1`;
  нужен именно cold boot, т.к. при resume guest-agent в снапшоте — старый
  процесс без debug-логов (лог пишется в `<share>/guest-agent.log`).
- `make service-debug-rebuild` — полная пересборка + debug-перезапуск.
- `make boot-containerd` — разовый boot в foreground (initramfs не
  пересобирается, чтобы не инвалидировать хеш снапшота).
- `make prune` — очистить контейнеры/volumes/образы внутри VM.
- `make disk-compact` — вернуть место на хосте после prune: sparse-копия
  `~/.anvil-vz/containerd-disk.img` (демон останавливается и запускается
  снова; логический размер и содержимое не меняются, снапшот остаётся
  валидным).
- `make clean` — удалить `.build`, `.download`, `.venv`.

Переменные окружения: `DEBUG=1` (debug-логи), `ANVIL_MEMORY` (ГБ RAM VM),
`ANVIL_DISK_GB` (размер containerd-диска, дефолт 64; существующий образ
только расширяется, следующий запуск — cold boot + online resize2fs),
`ANVIL_SHARE_USERS=0` (отключить virtiofs-шару host `/Users` в госте —
по умолчанию `/Users` смонтирован в VM по тому же пути, поэтому bind-mounts
`docker run -v $HOME/...:/path` и compose `volumes:` работают без
переписывания путей),
`VERSION` (подставляется в `version.swift`), `VZRUNNER_BIN` (путь к бинарнику
для сервисных скриптов и bench-harness).

## Тестирование

Автоматических unit-тестов нет (ни Swift, ни Go — `*_test.go` отсутствуют).
Проверка — интеграционная, через скрипты и ручной прогон Docker CLI:

- `make test` — smoke: сборка + `--help`.
- `make time-boot` / `make time-service` — замеры времени загрузки
  (`scripts/time_boot.py`, `scripts/time_service.py`).
- `make validate` — набор robustness-проверок
  (`scripts/validate_robustness.py`): save/resume циклы, resume с запущенным
  контейнером, kill -9 без orphan-процессов, утечки FD, CNI cleanup,
  изоляция двух проектов и конфликты портов.
- `make harness` / `make bench-all` — bench-harness (`bench-harness/`):
  сравнение cold start / resume / compose up с Lima, Colima, OrbStack,
  Docker Desktop; результаты в `bench-harness/results/`.

Любое изменение guest-agent или vz-runner проверяется реальным прогоном:
`make service-debug-rebuild`, затем `docker --context anvil run ...` /
`docker compose up` на тестовом стеке.

## Организация кода

- `Sources/vz-runner/` — Swift-исходники, один файл ≈ один компонент:
  `main.swift` (CLI: start/stop/status/daemon/boot/exec), `DaemonCommand`,
  `VMLifecycleManager`, `VMConfig`, `SnapshotManager`, `ControlServer` +
  `ControlClient` + `ControlProtocol`, `DockerProxyServer`, `PortForwarder`,
  `GuestCacheDropper`, `ContainerdCacheManager`, `BootCommand`.
- `guest-agent/` — Go-исходники по доменам Docker API: `main.go` (vsock
  control server, reaper зомби), `dockerapi.go` (HTTP-маршруты),
  `containers.go`, `images.go`, `networks.go`, `volumes.go`, `exec.go`,
  `archive.go`, `healthcheck.go`, `scanner.go` (port scanner/pusher),
  `buildkit.go` (vsock:1026 bridge к buildkitd + ленивый старт),
  `build.go` (`/build` через nerdctl), `info.go`, `utils.go`.
- `scripts/` — сборка initramfs и сервисная обвязка
  (`build_initramfs_containerd.sh` — основной, внутри него `stage2.sh` и
  `myinit` генерируются inline).
- `bench-harness/` — бенчмарки (`run_bench.sh`, драйверы бэкендов в
  `drivers/`, workload в `workloads/`).
- `networks/`, `var-lib/`, `guest-agent.log`, `.download/` — runtime-артефакты
  virtiofs share / состояния, не исходный код.

## Соглашения и важные инварианты

- Язык документации — русский (`ARCHITECTURE.md`, плановые документы).
  **Комментарии в коде — только английский** (`Sources/*.swift`,
  `guest-agent/*.go`, `scripts/*.sh`, `Makefile`, CI-workflows): русских
  комментариев в коде быть не должно, существующие переводим при касании.
  Документацию (`*.md`) пишите на русском.
- Swift: Foundation + Virtualization.framework, без сторонних зависимостей
  (в `Package.swift` нет dependencies — не добавляйте без необходимости).
  POSIX sockets вместо Network.framework — осознанное решение (см.
  `ARCHITECTURE.md` §3.4).
- Go: статический бинарник без CGO; guest-agent — PID 1, поэтому он **не
  должен падать** и обязан reap'ить зомби (`reapZombies`).
- Docker API эмулируется лишь частично — только то, что нужно `docker` и
  `docker compose`. Есть нетривиальные инварианты, которые нельзя ломать
  (подробности в `ARCHITECTURE.md` §4.3): детерминированный container ID
  (`sha256(namespace + "/" + containerdID)[:64]`); `/containers/{id}/wait`
  шлёт заголовки сразу (chunked) до блокировки; `AutoRemove` реализуется
  самим guest-agent, а не `nerdctl --rm`.
- Compose-network labels персистятся в `/mnt/anvil/networks/<name>.json`
  (nerdctl их не возвращает) — при изменении `networks.go` не потеряйте
  восстановление CNI conflist при cold boot.
- Хеш снапшота включает kernel/initrd/CPU/RAM/диск — изменение `VMConfig`
  или initramfs инвалидирует снапшот и приводит к cold boot; это ожидаемо,
  но учитывайте при отладке.
- Containerd-диск использует host writeback cache (`cachingMode: .cached`,
  `synchronizationMode: .none`) — осознанный trade-off durability↔скорость
  для dev-VM, не «исправляйте» без обсуждения (`ARCHITECTURE.md` §7.1).
- `version.swift` генерируется Makefile'ом; руками не редактировать.

## Безопасность

- Демон хранит состояние в `~/.anvil-vz/` (снапшоты, диск containerd,
  pid/логи, сохранённый docker context). Не удаляйте эти файлы молча —
  снапшот и диск содержат пользовательские данные.
- Control-сокет принимает команды `exec`, которые выполняются внутри VM с
  правами root; сокет не аутентифицируется (рассчитан на локального
  пользователя). Не выставляйте его наружу и не расширяйте протокол
  бездумно.
- `anvil-service.sh` предупреждает про `http_proxy` без `localhost` в
  `NO_PROXY` — не убирайте эту проверку.
- Релизы: `make release VERSION=x.y.z` (тег + GitHub Actions из
  `.github/workflows/go.yml` + обновление Homebrew tap). Git-мутации
  (tag/push) выполняются только по явной просьбе пользователя.

## Деплой / релизный процесс

CI — `.github/workflows/go.yml` (macos-15): проверка сборки на PR в `test`,
при пуше тега `v*` — сборка, codesign, публикация
`anvil-darwin-arm64.tar.gz` в GitHub Releases. Затем
`make update-brew VERSION=x.y.z` обновляет формулу в соседнем репозитории
`homebrew-tap` (заодно вычищает устаревший bottle-блок), и
`make bottle VERSION=x.y.z` (`scripts/make_bottle.sh`) собирает bottle
(`brew install --build-bottle` + `brew bottle --no-rebuild`), загружает
tarball в тот же GitHub release (имя файла с одинарным дефисом — `brew
bottle` создаёт с двойным, а brew ищет с одинарным), заливает тот же
tarball под остальные поддерживаемые macOS-теги и прописывает bottle-блок
со всеми тегами в формулу. Два инварианта, которые нельзя ломать:

- **`rebuild` обязан быть 0.** `brew bottle` без `--no-rebuild` выставляет
  rebuild = (rebuild формулы на origin/HEAD) + 1, а т.к. update-brew уже
  запушил очищенную формулу, upstream совпадает и rebuild становится 1.
  При rebuild > 0 zerobrew строит URL по своей несовместимой схеме
  `<name>-<version>.<rebuild>.<tag>.bottle.tar.gz` (у Homebrew rebuild
  после тега) и получает 404 — у него нет fallback на source.
- **Теги всех поддерживаемых macOS.** Bottle — `cellar
  :any_skip_relocation` и не содержит платформенно-специфичных
  артефактов, поэтому один tarball обслуживает все теги; они перечислены
  в цикле `EXTRA_TAGS` в `make_bottle.sh` — при выходе новой версии macOS
  добавьте её тег туда.
Установка пользователями — через Homebrew (`vz-runner` в
PATH + assets в `share/anvil`); LaunchAgent —
`scripts/com.olegshirko.anvil.plist` (`make service-install`).
