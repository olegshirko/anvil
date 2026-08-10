# План улучшений Anvil

Приоритеты: 1) скорость запуска, 2) баги и надёжность, 3) функциональность,
4) тесты и CI, 5) DX и релиз. Внутри разделов — по убыванию эффекта.

## 1. Скорость запуска

Базовые замеры (daemon.log / console.log, июль 2026):

- Холодный старт до готовности ≈ 8.3 с: VM start 0.12 с → guest-agent ready
  5.76 с → сохранение снапшота 2.44 с (инлайн, до объявления готовности).
- Resume из снапшота ≈ 1 с.

**Результат после первой итерации (сделано, bench-harness): холодный старт
daemon ready 1339 мс (честный cold boot, для сравнения OrbStack 1560 мс,
Docker Desktop 2175 мс), resume 568 мс (Colima 13301 мс, Lima 9114 мс),
guest-agent ready 1.16 с после VM start (было 5.4–5.8 с), initramfs
109 → 51 МБ, idle RSS 2766 → ~700 МБ.**

Сделано:

1. **Честные замеры.** `cmdStatus` выходил с кодом 0 всегда — теперь != 0,
   пока демон не поднялся и control-цепочка (daemon → control server →
   guest-agent health) не отвечает (`main.swift`). В bench-harness добавлен
   хук `backend_cold_reset` (удаляет снапшот vz-runner перед фазой cold
   start) — раньше «cold start» в бенче на деле был resume.
2. **Ленивый снапшот (−2.4 с на холодный старт).** `VMLifecycleManager`
   больше не делает pause → save → resume до `didBecomeReady`; снапшот
   сохраняется на idle-таймауте и при штатной остановке, как раньше.
3. **Кеш sha256 ассетов.** Хеш kernel+initrd (~172 МБ) считался дважды за
   запуск; теперь persistent-кеш по (path, size, mtime) в
   `~/.anvil-vz/asset-hashes.json` (`SnapshotManager.swift`).
4. **Гостевая загрузка.** Убран блокирующий `ntpd` (фон, RTC от VZ
   достаточно); `udhcpc -n -q` вместо опроса 0.5 с; poll `containerd.sock`
   0.5 → 0.1 с; `sleep 1` после killall — только если были убиты процессы.
5. **Похудение initramfs 109 → 51 МБ:** выкинуты `containerd-stress` и
   rootless-скрипты (−19 МБ), оставлены 6 нужных CNI-плагинов (−56 МБ),
   упаковка `gzip -9` → `zstd -19` (распаковка initrd 0.63 → 0.2 с),
   guest-agent с `-ldflags="-s -w"`.
6. **Прочее:** `random.trust_cpu=on` в cmdline (crng init был на ~81 с),
   быстрые опросы сокетов на хосте (0.5 → 0.1 с), `anvil-service.sh` без
   `python3` в циклах ожидания.
7. **Размер скачивания 126 → ~86 МБ:** ядро пакуется в релизный tarball
   сжатым (`vmlinuz-raw.gz`, 18.5 МБ вместо 59 МБ) и распаковывается один
   раз при первом старте в `~/.anvil-vz` (переупаковывается, когда gz из
   пакета новее — т.е. при апгрейде).

Осталось (отложено):

7. **Копия rootfs в tmpfs + switch_root (~0.3–0.5 с).** myinit копирует
   ~165 МБ в /newroot, т.к. rootfs initramfs — не отдельный mount и runc
   pivot_root ломается. Пока оставлено (рискованно), частично компенсировано
   похудением образа.
8. **(дорого, опционально) Alpine linux-virt ядро** вместо Ubuntu generic:
   образ в разы меньше, инициализация быстрее. Нужно подобрать набор модулей
   (ext4, vsock, virtiofs, bridge, netfilter).

## 1а. Переход на Alpine linux-virt (сделано)

- Ядро и модули берутся из **одного apk** `linux-virt-<ver>` (vermagic
  совпадает по построению; netboot-образы Alpine отстают от репо, поэтому
  ядро из netboot использовать нельзя). Ubuntu generic (59 МБ raw, 87 МБ
  modules.deb, хантинг версии по `strings`) полностью удалён из pipeline;
  ядро raw 59 → 34.4 МБ (~10 МБ в gzip), сборка детерминирована.
- Подводные камни linux-virt vs Ubuntu generic (все закрыты):
  - `CONFIG_VIRTIO_BLK/NET/FS/PACKET=m` (у Ubuntu — built-in): нужны модули
    `virtio_blk`, `virtio_net` (+`failover`/`net_failover`), `fuse`,
    `af_packet` (иначе нет диска, сети, шары и DHCP).
  - `libcrc32c` требует `crc32c` (softdep, busybox modprobe его не
    разрешает) → грузим `crc32c_generic` явно; без него не работают и
    nf_conntrack, и ext4 (metadata_csum).
  - Нет `RANDOM_TRUST_CPU` и virtio-rng в VZ: crng init занимал ~10 с.
    Решено сидированием из хоста: vz-runner пишет 64 байта в
    `.anvil-host-entropy`, guest-agent кредитит пул через RNDADDENTROPY.
- Загрузка модулей переведена с явных `insmod`-списков на `modprobe`
  (modules.dep из apk) — dependency-цепочки больше не ломаются.
- Итог: guest-agent ready 1.14 с (как у Ubuntu), вся матрица (DHCP, ext4,
  шара, порты, pull, save/load, compose, resume) зелёная.

## 2. Баги и надёжность

1. **~~virtiofs обрезает большие записи из гостя в шару~~ — расследовано,
   это не virtiofs.** Корень — гонка в transfer-service containerd 2.0.0 при
   `ctr images export`: в логе containerd видно
   `error copying stream: write ...: file already closed` — стрим закрывается
   до конца записи, файл обрезается в случайном месте хвоста, а exit code
   остаётся 0 (молчаливая порча данных). На tmpfs гонка почти не видна
   (0/12), на virtiofs проявляется из-за более медленных записей (2/2).
   Обычные записи любого размера чанка (4 КБ–4 МБ, с fsync и без) чисты.
   Вывод: для экспортов использовать `nerdctl save` (другой, надёжный путь —
   им и реализован `docker save`), `ctr images export` в шару не
   использовать. Отдельный фоллоу-ап: ~~обновить containerd/nerdctl в образе
   (сейчас 2.0.0/2.0.4)~~ — сделано: containerd 2.3.3, nerdctl 2.3.5,
   runc 1.5.1, CNI plugins 1.9.1. Апгрейд вскрыл две регрессии, обе
   исправлены: nerdctl 2.2+ убрал label `nerdctl/ports` (порты теперь читаются
   из networkstore — `/var/lib/nerdctl/*/containers/<ns>/<id>/network-config.json`,
   fallback в `guest-agent/scanner.go`) и в госте не хватало busybox-апплета
   `find` (boot-cleanup name-store молча не работал — добавлен в initramfs).
2. **~~`docker save` не реализован~~ — сделано.** `GET /images/{name}/get` и
   `/images/get?names=...` стримят `nerdctl save` (docker-формат, надёжный
   путь в отличие от `ctr images export` — см. п.1). Проверен round-trip
   save → load. Позже починен резолв имени без тега (`docker save foo`
   матчится на `docker.io/library/foo:latest`, а `:` в `host:5000/foo`
   больше не принимается за тег) — `findImageNamespace` в
   `guest-agent/images.go`.
3. **~~Права на сокеты и state-директорию~~ — сделано.** `control.sock` и
   `docker.sock` — 0600, `~/.anvil-vz` — 0700, `containerd-disk.img` — 0600
   (chmod после bind/создания, применяется при каждом старте).
4. ~~**Устаревший комментарий в Makefile** (target `prune`): «docker run --rm
   is not yet implemented»~~ — уже вычищен при одной из итераций; AutoRemove
   давно реализован (`guest-agent/containers.go:489`).
5. **~~Флаки первого `docker run` после холодного старта~~ — исправлено.**
   Корень был не в «первом запросе», а в `--rm`: AutoRemove удалял контейнер
   (и его json-file логи) сразу после выхода, пока attach ещё реплеил вывод
   → короткоживущие контейнеры (`echo ...`) теряли stdout с гонкой. Теперь
   attach трекается (attachBegin/End) и удаление ждёт drain до 30 с; плюс
   attach ждёт выхода контейнера из `created` (attach приходит до start).
   Проверено: 10/10 warm и 2/2 cold выводят корректно. Отдельно замечен
   редкий транзиент `nerdctl start failed (1): <id>` (~1/17 быстрых
   последовательных запусков, shim-уровень) — на текущем стеке
   (containerd 2.3.3 / nerdctl 2.3.5 / runc 1.5.1) не воспроизводится:
   ~440 прогонов (последовательные, параллельные, burst после resume)
   чистые. Оставлено под наблюдением.
6. **~~Resume compose up регресс в бенче (5567 мс)~~ — исправлено.** Корнем
   оказался дедлок в `runExec` (guest-agent): stdout и stderr дочернего
   процесса дренились последовательно — как только `nerdctl compose up`
   выдавал в stderr больше pipe-буфера (64 КБ) при открытом stdout, ребёнок
   навсегда висел в `pipe_write`. Теперь оба потока дренируются
   конкурентно. Побочно починен и запуск `vz-runner exec` для «шумных»
   команд. После фикса resume compose up ~1.3 с.
7. **Сборка initramfs в Lima не зависит от сети.** Раньше скрипт внутри
   Lima VM перезапускал себя во вложенном docker и делал `apk add` (нужен
   доступ к Alpine CDN) — при сломанной/ограниченной сети сборка умирала.
   Теперь на Linux-хосте (Lima) сборка идёт напрямую: пакеты ставятся только
   если отсутствуют, рабочие директории — в mktemp вместо корня ФС.

Сделано в ходе расследования `docker load` и итерации скорости (июль 2026,
уже в коде):

- `DockerProxyServer.swift`: дозапись буфера циклом в обоих направлениях —
  раньше частичный `write()` молча ронял хвост 64-КБ чанка.
- `canonicalizeImageRef` (`guest-agent/images.go`): короткое имя с тегом
  (`myimg:1`) ошибочно считалось registry-qualified из-за проверки `":"` в
  первом сегменте — нормализация до `docker.io/library/...` не срабатывала,
  `ensureImageInNamespace` не находил локальный образ и шёл в `nerdctl pull`.
  При доступном registry это был скрытый лишний pull на каждый `docker run`
  (латентность), без registry — падение создания контейнера. Добавлен и
  fallback на сырое имя (образы из OCI-архивов с сырым `ref.name` теперь
  алиасятся на каноническое имя).
- `/images/load` переписан на containerd Go-client (`client.Import`): тело
  стримится без временного файла — tar ~430 МБ раньше убивал tmpfs гостя
  («no space left on device»). gzip/zstd определяются автоматически. Ответ
  содержит строки `Loaded image: <ref>`, как у настоящего Docker. Архивы без
  аннотаций имени (типичный `buildx --output type=oci` без `-t`) раньше
  «загружались» молча, но образ был недоступен по имени и `docker run`
  уходил в registry pull — теперь таким манифестам выдаётся ref по дайджесту
  (`docker.io/imported/anvil-image:<digest12>`). Архивы с сырым именем
  (`myapp:1` без registry-префикса) регистрировались as-is, а nerdctl на
  inspect/run канонизирует короткие имена в `docker.io/library/...` →
  `GET /images/{name}/json` падал с «no such image», и CLI/compose уходили
  в pull (офлайн — фатально). Теперь при load дополнительно регистрируется
  канонический алиас (тот же target, тот же namespace).
- `anvil-service.sh`: в source-дереве свежие assets из `.download` и свежий
  бинарник `.build/release/vz-runner` теперь приоритетнее копий в
  `~/.anvil-vz` и PATH — иначе после `make rebuild-all` сервис продолжал
  грузить старый initramfs со старым guest-agent и старый brew-бинарник.
- DNS в госте: myinit больше не затирает `/etc/resolv.conf` хардкодом
  8.8.8.8 — берётся DNS из DHCP (VZ NAT-шлюз проксирует резолвер хоста,
  работает в VPN/ограниченных сетях), 8.8.8.8 только как запасной. Раньше
  на сетях с заблокированным внешним DNS pull'ы висли бессимптомно.
- `/images/load`: добавлен `client.WithSkipMissing()` — одноплатформенные
  экспорты с мультиархитектурным индексом (без чужих блобов) больше не
  падают с «content digest ... not found».
- **Корневой баг «docker run/compose тянет из registry после docker load»**:
  containerd content store — namespace'нутый. `ensureImageInNamespace`
  копировал только метаданные образа в проектный namespace compose, и запись
  указывала на невидимые блобы → nerdctl уходил в pull (HEAD на registry) →
  падение без доступа к registry (а с сетью — скрытый pull на каждый
  compose up). Теперь образ стримится между namespace'ами
  (`nerdctl save | ctr images import -`) — работает полностью офлайн
  (проверено с пустым resolv.conf).
- **Часы гостя**: VZ не гарантирует RTC — загрузка начиналась с 1970-01-01 и
  TLS падал («certificate is not yet valid»). Процессы myinit не переживают
  switch_root, поэтому ntpd из myinit не спасал. Теперь vz-runner пишет epoch
  в `<share>/.anvil-host-time` при старте VM, myinit ставит часы из файла,
  ntpd — только фоновая коррекция дрейфа в stage2, guest-agent перечитывает
  файл при старте и на каждый subscribe (покрывает resume со «замороженными»
  часами).
- `anvil-service.sh`: для brew-установок assets пакета приоритетнее копий в
  `~/.anvil-vz` (иначе старый initramfs молча перекрывал каждый апгрейд),
  плюс warning об игнорируемых shadow-файлах.

## 3. Функциональность

1. **~~`docker build`~~ — сделано (классический путь + buildx remote
   driver).** В initramfs добавлен buildkitd (v0.32.2) + buildctl (buildctl
   нужен: nerdctl build вызывает его как subprocess); guest-agent реализует
   классический `POST /build`: контекст распаковывается на persistent-диск
   (`/var/lib/anvil-build`), сборка через `nerdctl build`, прогресс — JSON
   stream как у Docker. buildkitd стартует лениво при первом билде (экономия
   ~50 МБ RSS). Дополнительно buildkitd-сокет проброшен на хост:
   `~/.anvil-vz/buildkit.sock` → vsock:1026 → `/run/buildkit/buildkitd.sock`;
   `anvil start` создаёт buildx builder `anvil-remote` (remote driver) и делает его
   активным (предыдущий builder сохраняется и восстанавливается на stop) —
   дефолтный `docker build` (buildx) работает из коробки; для импорта в
   image store нужен `--load` (compose делает это сам).
   Работают оба пути: remote driver (`docker buildx build`) и
   docker-container driver (голый `docker build` на desktop CLI тянет
   moby/buildkit как контейнер — для этого починены `PUT
   /containers/{id}/archive` на остановленном контейнере и два бага stdin в
   exec: дедлок `cmd.Wait()` и выброшенный stdin, ломавший `buildctl
   dial-stdio`).
2. **Rosetta для x86_64** (`VZRosettaDirectoryShare`, macOS 13+) — запуск
   amd64-образов почти нативно, как у Lima.
3. **~~Bind-mounts произвольных host-путей~~ — сделано.** vz-runner
   шарит host-директорию `/Users` вторым virtiofs-устройством (tag
   `macusers`), stage2 монтирует её в госте по тому же абсолютному пути
   `/Users` — `docker run -v $HOME/proj:/data` и compose `volumes:` с
   относительными путями работают без переписывания путей, как в Docker
   Desktop. Отключается `ANVIL_SHARE_USERS=0`. Набор шар входит в хеш
   снапшота (добавление устройства ломает restore) → один cold boot после
   апгрейда. Попутно: guest-agent раньше молча игнорировал
   `HostConfig.Binds`/`Mounts` — теперь пробрасывает в `nerdctl -v`
   (включая named volumes); реализован `GET /events` (стрим task-events
   containerd в формате Docker: create/start/die с exitCode/destroy),
   без него `docker compose up` получал 404.
4. ~~**Догнать заглушки**: dangling images prune и build cache prune~~ —
   сделано. `/images/prune` удаляет dangling-образы по умолчанию и все
   неиспользуемые при `dangling=false` (`docker system prune -a`), считает
   SpaceReclaimed по размерам из `listDockerImages`. `/build/prune` гоняет
   `buildctl prune` по buildkit-сокету (buildkitd при этом НЕ стартует — нет
   демона, нет и кеша) и парсит reclaimed из строки `Total:`.

## 4. Тесты и CI

1. **CI-прогон robustness.** Job `validate` добавлен, но выяснилось, что
   GitHub-hosted macos-15 раннеры не предоставляют гипервизор
   (`VZErrorDomain Code=2 "Virtualization is not available on this
   hardware"`) — boot-тесты там не работают принципиально. Поэтому validate
   настроен на self-hosted Apple Silicon runner с запуском через
   workflow_dispatch; release от него не зависит. Локально —
   `make validate`.
2. **Go unit-тесты guest-agent** на инварианты из ARCHITECTURE.md §4.3:
   детерминированный container ID, `/containers/{id}/wait` (заголовки до
   блокировки), жизненный цикл AutoRemove. `httptest` по хендлерам.
3. **Swift-тесты**: парсинг `ControlProtocol`, хеш снапшота.

## 5. DX и релиз

1. ~~**`anvil doctor` / `anvil logs`**~~ — сделано: `doctor` проверяет
   гипервизор, entitlement, наличие kernel/initramfs/диска, свободное место,
   демон, docker context и ответ API на `/_ping`, шару `/Users`
   (exit code ≠ 0 при ошибках); `logs [daemon|console|guest]` показывает
   хвосты логов одной командой.
2. ~~**Ротация `guest-agent.log`**~~ — сделано: в debug-режиме guest-agent
   сам управляет файлом на шаре (`logrotate.go`): dup3 stdout/stderr на
   свой дескриптор и ротация по размеру — при превышении 50 МиБ текущий лог
   уходит в `guest-agent.log.1` (один бэкап), проверка раз в минуту. Максимум
   на хосте ~100 МиБ независимо от длины debug-сессии.
3. **Developer ID + нотаризация** — сейчас `spctl` отклоняет бинарник;
   для brew неважно, но ручное скачивание tarball упирается в Gatekeeper.
   Требует платного Apple Developer.
4. ~~**Homebrew bottle** вместо source-формулы~~ — сделано: в формуле
   bottle-блок (`root_url` на GitHub release, `cellar: :any_skip_relocation`,
   тег `arm64_tahoe`), bottle-тарболл публикуется ассетом релиза. Автоматизация:
   `make bottle VERSION=x.y.z` (`scripts/make_bottle.sh`) — переустанавливает
   формулу с `--build-bottle`, гоняет `brew bottle`, переименовывает tarball
   (brew ищет ассет с одинарным дефисом `anvil-1.0.x...`, а `brew bottle`
   создаёт с двойным), заливает в release через `gh`, обновляет bottle-блок
   и пушит tap. `make update-brew` при бампе версии вычищает устаревший
   bottle-блок (иначе rebuild инкрементируется от старого). На macOS старше
   tahoe тег бутылки не совпадёт — brew молча вернётся на source-формулу,
   что работает идентично (та же распаковка файлов).
5. ~~**Компакция containerd-диска**~~ — сделано (§5.5): дефолтный размер
   поднят 16→64 ГиБ (sparse, настраивается `ANVIL_DISK_GB`), существующий
   образ автоматически расширяется при старте (хеш снапшота включает размер
   файла → следующий запуск cold boot, stage2 догоняет ext4 через online
   `resize2fs`; mkfs.ext4/resize2fs теперь musl-бинарники из Alpine apks
   e2fsprogs/e2fsprogs-extra, а не glibc-копии из build-VM). Для возврата
   места на хосте после `make prune` добавлен `make disk-compact`
   (sparse-копия через `dd conv=sparse`, содержимое и логический размер не
   меняются — снапшот остаётся валидным). Плюс автоматика: virtio-blk в VZ
   поддерживает discard, поэтому guest-agent раз в сутки гоняет
   `fstrim /var/lib/containerd` (`periodicFstrim` в `main.go`, апплет busybox
   добавлен в initramfs) — дырки в sparse-образе пробиваются сами, без
   остановки демона.
