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

Осталось (отложено):

7. **Копия rootfs в tmpfs + switch_root (~0.3–0.5 с).** myinit копирует
   ~165 МБ в /newroot, т.к. rootfs initramfs — не отдельный mount и runc
   pivot_root ломается. Пока оставлено (рискованно), частично компенсировано
   похудением образа.
8. **(дорого, опционально) Alpine linux-virt ядро** вместо Ubuntu generic:
   образ в разы меньше, инициализация быстрее. Нужно подобрать набор модулей
   (ext4, vsock, virtiofs, bridge, netfilter).

## 2. Баги и надёжность

1. **(критично) virtiofs обрезает большие записи из гостя в шару.**
   Воспроизведение: `ctr images export /mnt/anvil/...` дважды дал файлы
   4194304 и 4174848 байт вместо правильных 4202496 (теряются хвостовые
   8–27 КБ), импорт такого tar падает с «failed to read expected number of
   bytes: unexpected EOF». Тот же экспорт во внутренний /tmp гостя — валиден,
   `cp` валидного файла в шару — тоже валиден. Затронуты `docker cp` наружу
   и любые большие записи в шару. Разобраться (похоже на потерю хвостовых
   записей при flush в VZ virtiofs).
2. **`docker save` не реализован**: `GET /images/{name}/get` → 404.
   Реализовать стримингом `ctr images export` в ответ (и `/images/get` для
   нескольких имён).
3. **Права на сокеты и state-директорию.** После `bind` — `chmod 0600` на
   `control.sock` и `docker.sock`, `0700` на `~/.anvil-vz`. Сейчас любой
   локальный пользователь может подключиться к Docker API — это root в VM.
4. **Устаревший комментарий в Makefile** (target `prune`): «docker run --rm
   is not yet implemented» — AutoRemove давно реализован
   (`guest-agent/containers.go:489`).
5. **Флаки первого `docker run` после холодного старта**: однократно
   наблюдались потеря вывода attach и ошибка разбора аргументов CLI
   («Run 'docker run --help'») на первом запросе к только что поднятому
   демону; повторный запуск работает. Похоже на race при первом
   vsock-соединении — воспроизвести и разобраться.
6. **Resume compose up регресс в бенче (5567 мс, было ~460 мс)** — после
   resume стек поднимается заметно дольше, чем раньше; проверить, не
   пересоздаются ли контейнеры вместо no-op.

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
  (`docker.io/imported/anvil-image:<digest12>`).
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

## 3. Функциональность

1. **`docker build`** — главный функциональный пробел (в `dockerapi.go` нет
   `/build`, только заглушка `/build/prune`). Путь: buildkitd в initramfs +
   проксирование `/build` и сессий. Временная мера: задокументировать
   `docker buildx` с внешним builder + `docker load`.
2. **Rosetta для x86_64** (`VZRosettaDirectoryShare`, macOS 13+) — запуск
   amd64-образов почти нативно, как у Lima.
3. **Bind-mounts произвольных host-путей** — сейчас один virtiofs share.
   Список дополнительных шар в `VMConfig` (учесть инвалидацию снапшота).
4. **Догнать заглушки**: dangling images prune и build cache prune возвращают
   пустое (`dockerapi.go:447,457`).

## 4. Тесты и CI

1. **CI-прогон на macos-15**: `make test` + `scripts/validate_robustness.py`
   на каждый PR (Virtualization.framework на раннерах работает, ad-hoc
   подписи достаточно). Сейчас workflow только компилирует Swift.
2. **Go unit-тесты guest-agent** на инварианты из ARCHITECTURE.md §4.3:
   детерминированный container ID, `/containers/{id}/wait` (заголовки до
   блокировки), жизненный цикл AutoRemove. `httptest` по хендлерам.
3. **Swift-тесты**: парсинг `ControlProtocol`, хеш снапшота.

## 5. DX и релиз

1. **`anvil doctor` / `anvil logs`** — проверка entitlement, состояния
   снапшота, docker context, портов; доступ к логам одной командой.
2. **Ротация `guest-agent.log`** — в debug-режиме растёт без ограничений.
3. **Developer ID + нотаризация** — сейчас `spctl` отклоняет бинарник;
   для brew неважно, но ручное скачивание tarball упирается в Gatekeeper.
   Требует платного Apple Developer.
4. **Homebrew bottle** вместо source-формулы — быстрее установка, надёжнее
   под zerobrew-подобными менеджерами.
5. **Компакция containerd-диска** — sparse-файл 16 ГиБ со временем разрастается
   физически: `fstrim` в госте, автоподсказка `make prune`.
