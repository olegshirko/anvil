# План улучшений Anvil

Приоритеты: 1) скорость запуска, 2) баги и надёжность, 3) функциональность,
4) тесты и CI, 5) DX и релиз. Внутри разделов — по убыванию эффекта.

## 1. Скорость запуска

Базовые замеры (daemon.log / console.log, июль 2026):

- Холодный старт до готовности ≈ 8.3 с: VM start 0.12 с → guest-agent ready
  5.76 с → сохранение снапшота 2.44 с (инлайн, до объявления готовности).
- Resume из снапшота ≈ 1 с.
- Внутри гостя: ядро до myinit 0.64 с; mount ext4 на 3.29 с (≈2.6 с уходит на
  DHCP + ntpd + копию rootfs в tmpfs); containerd → guest-agent ≈ 2.4 с.

Шаги (ожидаемый суммарный эффект: холодный старт ~2.5–3.5 с):

1. **Ленивое сохранение снапшота (−2.4 с).** `VMLifecycleManager.swift:293-337`:
   сейчас после готовности guest-agent VM встаёт на паузу, снапшот пишется
   2.4 с, потом resume — и только затем `didBecomeReady`. Сохранять снапшот
   после `didBecomeReady` (в фоне или на первом idle), готовность не блокировать.
2. **Убрать блокирующий ntpd.** myinit (`build_initramfs_containerd.sh` ~443):
   `ntpd -nq -p pool.ntp.org` выполняется синхронно после DHCP без таймаута.
   RTC от Virtualization.framework уже даёт правильное время — убрать совсем
   или в фон с `timeout 2`.
3. **DHCP без медленного опроса.** myinit (~432-436): poll `sleep 0.5` ×10.
   `udhcpc -n -q` в foreground обычно отрабатывает <200 мс на VZ NAT,
   либо опрос каждые 50–100 мс.
4. **Не копировать rootfs в tmpfs (−0.5–1 с).** myinit делает `cp -a` ~246 МБ
   в /newroot + switch_root на каждом холодном старте. Выполнять stage2 прямо
   из корня initramfs.
5. **Похудение initramfs 109 → ~50 МБ.**
   - `containerd-stress` (18.5 МБ) в `opt/containerd/bin` не используется;
   - 18 CNI-плагинов (78 МБ), реально нужны ~5 (bridge, portmap, firewall,
     tuning, host-local) → −56 МБ;
   - `gzip -9` → `zstd` (ядро поддерживает, распаковка быстрее);
   - guest-agent собирать с `-ldflags="-s -w"`.
6. **Мелкие задержки stage2.** Poll `containerd.sock`: `sleep 0.5` → 0.05–0.1 с;
   безусловный `sleep 1` после `killall -9` — только если реально были убиты
   процессы.
7. **Хеш снапшота.** `SnapshotManager.swift:81-87` читает и хеширует
   ~172 МБ (kernel+initrd), и это происходит дважды за запуск
   (`VMLifecycleManager.swift:173,223`). Кешировать sha256 по mtime/size,
   считать один раз.
8. **Cmdline ядра и энтропия.** Добавить `quiet loglevel=3` (меньше записей
   в serial-консоль) и `random.trust_cpu=on` + virtio-entropy — сейчас
   `crng init done` только на ~81-й секунде, что может тормозить первые
   TLS-операции контейнеров.
9. **Честные замеры (сделать первым).** `cmdStatus` всегда выходит с кодом 0
   (`main.swift:248-257`) — readiness-gate в bench-harness (`vz-runner status`)
   ничего не ждёт, а «cold start 1779 мс» в `bench-harness/results/latest.md`
   на деле является resume (харнес не удаляет снапшоты между фазами). Исправить
   exit-код `status` до готовности и добавить в харнес фазу настоящего
   холодного старта.
10. **(дорого, опционально) Alpine linux-virt ядро** вместо Ubuntu generic:
    образ в разы меньше, инициализация быстрее. Нужно подобрать набор модулей
    (ext4, vsock, virtiofs, bridge, netfilter).

Не на горячем пути, но бесплатно: `anvil-service.sh` на каждой итерации
ожидания сокета запускает `python3`; три последовательных вызова
`docker context rm/create/use` в `cmdStart` (~0.3–0.5 с) можно заменить на
проверку текущего контекста.

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

Сделано в ходе расследования `docker load` (июль 2026, уже в коде):

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
- `anvil-service.sh`: в source-дереве свежие assets из `.download` теперь
  важнее копий в `~/.anvil-vz` — иначе после `make rebuild-all` сервис
  продолжал грузить старый initramfs со старым guest-agent.

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
