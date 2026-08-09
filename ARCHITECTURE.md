# Anvil vz-runner: архитектура и ключевые решения

Этот документ описывает, из каких компонентов состоит система, почему выбраны именно эти технологии, и как всё ведёт себя при старте, перезапуске и остановке.

## 1. Высокоуровневая картина

```
┌─────────────────────────────────────────────────────────────────┐
│                        macOS (host)                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐  │
│  │   vz-runner  │  │   Docker CLI │  │  docker compose CLI  │  │
│  │   (Swift)    │  │              │  │                      │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────┬───────────┘  │
│         │                 │                      │              │
│         │ unix socket     │ ~/.anvil-vz/docker.sock             │
│         │ ~/.anvil-vz/    │                      │              │
│         │ control.sock    │                      │              │
│         │                 │                      │              │
│  ┌──────▼─────────────────▼──────────────────────▼───────────┐  │
│  │              DockerProxyServer / ControlServer            │  │
│  │                    (внутри vz-runner)                     │  │
│  └──────┬─────────────────────────────┬────────────────────┘  │
│         │ vsock:1024                  │ vsock:1025            │
│         │ control channel             │ Docker API channel    │
│         │                             │                       │
│  ┌──────▼─────────────────────────────▼────────────────────┐  │
│  │                     Linux VM (guest)                    │  │
│  │  ┌─────────────────────────────────────────────────┐   │  │
│  │  │  guest-agent (Go)                               │   │  │
│  │  │  • vsock control server                         │   │  │
│  │  │  • Docker API server                            │   │  │
│  │  │  • port scanner + pusher                        │   │  │
│  │  │  • healthcheck runner                           │   │  │
│  │  │  • CNI config generator                         │   │  │
│  │  └────────────────────┬────────────────────────────┘   │  │
│  │                       │                                  │  │
│  │  ┌────────────────────▼────────────────────────────┐   │  │
│  │  │  containerd + nerdctl + CNI plugins             │   │  │
│  │  │  /var/lib (containerd + nerdctl volumes)        │   │  │
│  │  │  на virtio-blk диске                            │   │  │
│  │  └─────────────────────────────────────────────────┘   │  │
│  └─────────────────────────────────────────────────────────┘  │
│                          │                                     │
│              VZVirtioFileSystemDeviceConfiguration            │
│                          │ /mnt/anvil                          │
│         ┌────────────────┴────────────────┐                   │
│         │   virtiofs share (host dir)       │                   │
│         │   • guest-agent.log (debug)       │                   │
│         │   • networks/<name>.json          │                   │
│         │   • containerd-cache fallback     │                   │
│         └───────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────┘
```

`vz-runner` — единственный долгоживущий host-процесс. Он:

- создаёт и управляет Linux VM через `Virtualization.framework`;
- открывает unix socket для control plane и Docker API proxy;
- слушает vsock с guest-agent;
- открывает TCP-listener'ы на `localhost:<hostPort>` для проброски портов контейнеров;
- сохраняет и восстанавливает VM-снапшот.

`guest-agent` — единственный процесс внутри VM после `stage2`. Он:

- принимает команды от `vz-runner` по vsock (exec, статус, sync);
- эмулирует Docker API для Docker CLI / docker compose;
- сканирует containerd и пушит в `vz-runner` актуальные port mappings;
- сам запускает healthcheck'и, потому что `nerdctl` не поддерживает Docker-флаги `--health-cmd`;
- генерирует CNI-конфиги для per-project bridge-сетей.

## 2. Почему не Lima / Docker Desktop / OrbStack

Изначально Anvil использовал Lima. Lima хороша для прототипов, но для продукта даёт много лишнего: SSH-демон, systemd, cloud-init, guest-agent Lima, слой gvisor-tap-vsock. Это замедляет cold boot и усложняет snapshot.

Решение — свой минимальный guest rootfs и собственный host-раннер:

- **Нет systemd.** Init — busybox shell, stage2 запускает containerd и guest-agent напрямую.
- **Нет SSH.** Управление идёт через vsock, а не TCP/SSH.
- **Нет gvisor-tap-vsock.** Сеть — встроенный `VZNATNetworkDeviceAttachment` от Apple.
- **Single shared VM.** Все проекты пользователя живут в одной VM, но изолированы через containerd namespaces и отдельные CNI bridge.

## 3. Host-сторона: vz-runner

### 3.1 VMLifecycleManager

Отвечает за весь жизненный цикл VM:

- **cold boot** — создаёт `VZVirtualMachineConfiguration`, стартует VM, ждёт guest-agent;
- **resume** — `restoreMachineStateFromURL` из `~/.anvil-vz/snapshots/default.vzstate`;
- **pause + save** — при idle timeout или SIGTERM: pause VM, сброс page cache в guest, сохранение state;
- **snapshot invalidation** — перед restore вычисляется хеш от kernel, initrd, CPU, RAM, disk path/size. Если конфигурация изменилась — cold boot и пересоздание снапшота.

### 3.2 ControlServer

Unix socket `~/.anvil-vz/control.sock`. Принимает length-prefixed JSON от CLI и пересылает его в guest-agent через vsock:1024. Используется для `vz-runner exec`, `vz-runner status`, sync containerd cache. При `--debug` печатает команду и exit code каждого запроса.

`DockerProxyServer` раньше шёл мимо ControlServer напрямую к vsock, из-за чего при idle-pause Docker CLI получал `EOF`. Теперь DockerProxyServer тоже вызывает `ensureRunning`, чтобы возобновить VM перед принятием docker-запроса.

### 3.3 DockerProxyServer

Unix socket `~/.anvil-vz/docker.sock`. Принимает HTTP от Docker CLI, убирает префикс `/v1.XX`, пересылает raw bytes в vsock:1025, где работает HTTP-сервер guest-agent. Ответ возвращается обратно без парсинга протокола.

Тот же класс (`DockerProxyServer` с параметром порта) обслуживает `~/.anvil-vz/buildkit.sock` → vsock:1026: сырой TCP-мост к unix-сокету buildkitd в госте. На него указывает buildx builder `anvil-remote` (remote driver), который создаётся/восстанавливается при старте демона (`anvil-service.sh` + `main.swift`). Через него работают `docker buildx build` и `docker compose build` без промежуточного buildkit-контейнера. buildkitd в госте стартует лениво — guest-agent поднимает его при первом подключении к порту 1026 или первом `nerdctl build`.

### 3.4 PortForwarder

Guest-agent сканирует запущенные контейнеры и пушит в `vz-runner` полный список port mappings. `PortForwarder`:

- открывает `listen 0.0.0.0:<hostPort>`;
- форвардит TCP на `<guestIP>:<hostPort>` (не на container port, а на published host port внутри guest);
- при каждом пуше делает full-state replace: новые порты открываются, исчезнувшие закрываются;
- при попытке открыть уже занятый порт логирует конфликт.

Выбраны POSIX sockets, а не `NWListener`, потому что нужен простой TCP proxy loop без TLS/path monitoring/event-loop overhead Network.framework.

### 3.5 SnapshotManager

Сохраняет/восстанавливает `default.vzstate`. Перед сохранением `GuestCacheDropper` запускает в guest `sync; echo 3 > /proc/sys/vm/drop_caches`, чтобы снапшот не тащил page cache образов.

## 4. Guest-сторона: guest-agent

### 4.1 Запуск и роль PID 1

`stage2.sh` делает `exec /bin/guest-agent`. Процесс становится PID 1, поэтому:

- запущен фоновый reaper `SIGCHLD` через `syscall.Wait4(-1, WNOHANG)`, иначе завершившиеся `containerd-shim`/`runc`/`nerdctl` превращаются в зомби и дедлокают containerd;
- guest-agent не должен неожиданно падать — иначе VM остаётся без управления.

### 4.2 vsock control channel (порт 1024)

Простой length-prefixed JSON. Команды: `exec`, `status`, `sync`. Ответ содержит stdout/stderr/exit_code. Используется как CLI control plane.

### 4.3 Docker API server (порт 1025)

HTTP/1.1 сервер, эмулирует подмножество Docker API, необходимое для `docker` и `docker compose`:

- `/_ping`, `/version`;
- `/containers/*` create/start/stop/wait/rm/attach/logs/exec/inspect/archive;
- `/images/*` create/json/inspect/tag/push/rmi;
- `/networks/*` create/inspect/list/rm/prune;
- `/volumes/*` create/inspect/list/rm/prune.

Особенности:

- Docker container ID — детерминированный `sha256(namespace + "/" + containerdID)[:64]`, чтобы ID не менялся между сессиями.
- `POST /containers/{id}/wait` отправляет HTTP-заголовки сразу (chunked encoding) и только потом блокируется до выхода контейнера. Это нужно, потому что Docker CLI вызывает `/wait` до `/start`, и если ответ не начать сразу, следующий `/start` встаёт в очередь на том же соединении и контейнер никогда не стартует.
- `HostConfig.AutoRemove` не передаётся в `nerdctl create --rm`. Вместо этого guest-agent сам удаляет контейнер после того, как сохранил exit code, иначе `docker run --rm` не мог бы вернуть ненулевой код возврата.
- На hijacked exec/attach-соединении stdin клиента — всегда сырой байтовый поток (мультиплексируется только вывод). Его нужно форвардить в процесс как есть: buildx docker-container driver гоняет gRPC через `buildctl dial-stdio` по этому каналу. При этом `cmd.Wait()` ждёт и копирование stdin, поэтому stdin-pipe закрывается по EOF выходных потоков — иначе клиент, держащий соединение открытым (buildx), дедлокает exec.

### 4.3a buildkit bridge (порт 1026)

`buildkit.go`: raw TCP-мост vsock:1026 ↔ `/run/buildkit/buildkitd.sock`. Первое входящее подключение (или `POST /build`) лениво стартует `buildkitd`. Порт проброшен на хост как `~/.anvil-vz/buildkit.sock` (см. §3.3).

### 4.4 Port scanner

Подключается к containerd через `/run/containerd/containerd.sock`, периодически читает задачи и их labels. При изменении портов:

- при старте и после resume — пушит full state;
- при обычных изменениях — debounce 150 мс, затем пушит итоговое состояние.

### 4.5 Healthcheck

Guest-agent сохраняет healthcheck-конфиг из `POST /containers/create` и сам запускает периодические `nerdctl exec` проверки. Статус `(healthy)`/`(unhealthy)`/`(starting)` возвращается в `docker ps` и `docker inspect`, что позволяет `docker compose` использовать `depends_on: condition: service_healthy`. Изначально так было сделано, потому что `nerdctl` 2.0.4 не поддерживал `--health-cmd`; в 2.3.5 флаги появились, но собственный раннер оставлен — он уже проверен и не зависит от особенностей nerdctl.

### 4.6 CNI / per-project networking

Каждый Docker Compose project получает свой namespace и bridge-сеть:

- subnet детерминированный: `10.10.<hash(project) % 250 + 1>.0/24`;
- bridge `br-<sanitized-project>`;
- CNI conflist `/etc/cni/net.d/nerdctl-<name>.conflist` генерируется guest-agent перед созданием контейнера или сети.

Compose-метки (`com.docker.compose.project`, `com.docker.compose.network` и др.) сохраняются в `/mnt/anvil/networks/<name>.json`, потому что `nerdctl network inspect` для bridge-сетей не возвращает labels. Эти labels мерджатся в ответы `GET /networks`, а при старте guest-agent восстанавливает CNI conflist из сохранённых labels — иначе после cold boot Compose считает сеть "external" и отказывается её использовать.

### 4.7 Image canonicalization

Docker CLI часто передаёт неполные refs (`postgres:15.5`). `nerdctl` хранит образы под полным именем (`docker.io/library/postgres:15.5`). Guest-agent канонизирует ref перед pull/lookup, чтобы `docker compose` не получал `no such image`.

## 5. Initramfs и stage2

### 5.1 Структура

База — Alpine initramfs-virt. Внутри Linux-контейнера (`alpine:3.20`) собирается rootfs:

- busybox + базовые applets;
- containerd, nerdctl, runc, CNI plugins;
- guest-agent;
- Ubuntu kernel modules: vsock, virtiofs, overlayfs, bridge, veth, netfilter/xt/nft модули;
- iptables + libmnl/libnftnl/libxtables;
- GNU tar + libacl/libattr для `nerdctl cp`.

### 5.2 Почему switch_root, а не bind/pivot

Ранее делались bind-mount'ы в `/newroot`, но `pivot_root` нельзя вызвать из initramfs pseudo-fs `rootfs`. `switch_root` (busybox) делает `mount --move` + `chroot` + `exec`, что корректно переносит init в tmpfs root.

### 5.3 stage2

`stage2.sh` после `switch_root`:

1. Проверяет/перемонтирует proc/sys/dev/cgroup;
2. Монтирует virtiofs share в `/mnt/anvil`;
3. Монтирует `/var/lib` целиком:
   - первый приоритет — virtio-blk диск `/dev/vda` (ext4);
   - второй — bind-mount `/mnt/anvil/var-lib` (fallback);
   - третий — tmpfs (последний fallback).

   Это важно: раньше диск монтировался только на `/var/lib/containerd`, а
   `/var/lib/nerdctl` (volumes, metadata) и `/var/lib/cni` оставались на
   tmpfs-root. При заполнении volumes (например, PostgreSQL) tmpfs
   заканчивалась, и `docker ps` тормозил из-за переполненного/фрагментированного
   `nerdctl`-state. Теперь весь `/var/lib` persistent.
4. Загружает netfilter/bridge/veth модули;
5. Добавляет iptables MASQUERADE для DNATed TCP (исправляет asymmetric routing при VZ NAT);
6. Стартует containerd;
7. Чистит orphaned контейнеры через low-level `ctr` (не `nerdctl rm`, чтобы не ждать зависший shim);
8. Стартует guest-agent.

## 6. Поведение при перезапуске

### 6.1 Сервисный запуск

`scripts/anvil-service.sh`:

- сохраняет текущий docker context (если он не `anvil`);
- запускает `vz-runner daemon` с нужными путями к kernel/initrd/disk/share;
- передаёт `--debug`, если `DEBUG=1`;
- ждёт появления `~/.anvil-vz/control.sock`;
- делает `docker context use anvil`;
- предупреждает, если в окружении установлен `http_proxy`, а `NO_PROXY` не содержит `localhost/127.0.0.1` (иначе `curl localhost:<port>` и интеграционные тесты могут уйти на прокси).

#### Debug-режим

`DEBUG=1 make service-start` включает debug-логи только host-стороны
(`vz-runner`), потому что `service-start` делает **resume** из снапшота.
Guest-agent внутри снапшота — это старый процесс, который стартовал без
`ANVIL_DEBUG=1` и не перечитывает `.anvil-debug` при resume. Поэтому
`guest-agent.log` не обновляется.

Чтобы получить debug-логи guest-agent:

```bash
make service-debug       # stop + удалить снапшот + cold boot с DEBUG=1
```

На cold boot `stage2.sh` видит `/mnt/anvil/.anvil-debug` и запускает
`guest-agent` с `ANVIL_DEBUG=1`, пишет в `<share>/guest-agent.log`.
Последующие `make service-start`/`service-stop` будут resume'ить VM уже в
debug-состоянии, пока не будет создан новый снапшот без debug.

При остановке:

- SIGTERM демону;
- восстановление предыдущего docker context.

### 6.2 Cold boot

1. `vz-runner` не находит snapshot или хеш не совпадает — cold boot.
2. VM стартует с kernel + initramfs.
3. `myinit` → `switch_root` → `stage2`.
4. Containerd поднимается с persistent disk.
5. Guest-agent стартует, восстанавливает CNI conflists из `/mnt/anvil/networks/`.
6. Guest-agent пушит full port state (пока пустой).
7. `vz-runner` сохраняет снапшот.

### 6.3 Resume

1. `vz-runner` находит валидный snapshot.
2. `restoreMachineStateFromURL` — обычно < 1 с.
3. VM продолжает выполнение с того места, где была пауза.
4. Guest-agent переподключается к vsock и пушит full port state.
5. `PortForwarder` заново открывает нужные listener'ы.

### 6.4 SIGTERM / idle timeout

1. `vz-runner` получает SIGTERM или idle timer срабатывает.
2. Запускается `ContainerdCacheManager.sync()` на фоновой очереди (раньше блокировал main queue).
3. `GuestCacheDropper` дропает page cache в guest.
4. VM pause + `saveMachineStateToURL`.
5. Процесс завершается.

### 6.5 Перезапуск демона

Если `vz-runner` перезапустился, а VM не была сохранена, происходит cold boot. Если snapshot сохранён — resume. Serial port в daemon mode перенаправлен в `~/.anvil-vz/console.log`, чтобы `VZFileHandleSerialPortAttachment` на stdio не ломал restore из-за новых FD нового процесса.

## 7. Постоянное хранилище

### 7.1 Containerd root

Используется virtio-blk диск `~/.anvil-vz/containerd-disk.img` (ext4). Почему:

- **tmpfs** — образы жили в RAM, снапшот раздувался, cold boot требовал tarball restore;
- **loop-файл на virtiofs** — давал `input/output error` на `meta.db` и медленную запись (1.7 GB tarball ~23 с);
- **virtio-blk** — нормальная файловая система, образы переживают reboot, resume быстрый.

#### Writeback cache и synchronization mode

По умолчанию `VZDiskImageStorageDeviceAttachment` работает в режиме `.full`:
каждый guest fsync вызывает flush на хосте. Для `nerdctl`/containerd, которые
постоянно пишут мелкие metadata-операции, это приводит к тому, что
`docker stop`/`docker compose down` занимают по 10 с (graceful timeout),
а `docker run --rm alpine` — 4+ с.

Решение — включить хостовый writeback cache и отключить guest-fsync:

```swift
let attachment = try VZDiskImageStorageDeviceAttachment(
    url: diskURL,
    readOnly: false,
    cachingMode: .cached,
    synchronizationMode: .none
)
```

После этого:

- `docker ps` — ~0.06 s;
- `docker images` — ~0.04 s;
- `docker run --rm alpine echo hi` — ~2.0 s;
- `make run_tests_locally` в `pprb_uzp_efficiency` — ~50 s.

Риск: при kill -9 демона или panic на хосте может потеряться последняя
транзакция metadata. Для dev-VM приемлемо; production-нагрузки этот режим не
подходит. Долговечность обеспечивается snapshot save/resume в штатном сценарии.

#### Preallocated raw disk + ext4 / virtio-blk tuning

Изначально использовался `.dmg` (UDIF sparse-образ). На random writes APFS
добавляет overhead на выделение блоков за EOF. Перешли на raw image,
созданный полным заполнением нулями:

```bash
dd if=/dev/zero of="$HOME/.anvil-vz/containerd-disk.img" bs=1m count=16384
limactl shell anvil -- sudo mkfs.ext4 -F "$HOME/.anvil-vz/containerd-disk.img"
```

Stage2 монтирует его с опциями, минимизирующими metadata-задержки:

```bash
mount -t ext4 -o noatime,nobarrier,data=writeback,commit=60 /dev/vda /var/lib
```

И тюнит блочное устройство:

```bash
echo none > /sys/block/vda/queue/scheduler
echo 256 > /sys/block/vda/queue/read_ahead_kb
echo 256 > /sys/block/vda/queue/nr_requests
echo 2 > /sys/block/vda/queue/nomerges
```

Результат по сравнению с `.none` без tuning:

- `docker ps` — ~0.01 s (было ~0.06 s);
- `docker images` — ~0.03 s (было ~0.04 s);
- `docker run --rm alpine echo hi` — ~1.2 s (было ~2.0 s);
- `make run_tests_locally` — ~40 s (было ~50 s).

> **Multiqueue virtio-blk** — в текущем Virtualization.framework SDK
> (`VZVirtioBlockDeviceConfiguration.h`, macOS 14/15) public property для
> количества очередей отсутствует, поэтому этот пункт не применим без
> обновления Xcode/SDK.

### 7.2 Virtiofs share

`/mnt/anvil` на хостовой директории проекта. Используется для:

- debug-лога guest-agent;
- persisted network labels (`/mnt/anvil/networks/`);
- fallback `/var/lib` (`/mnt/anvil/var-lib`), если virtio-blk диск не указан;
- one-time миграции старого tarball cache в `/mnt/anvil/var-lib/containerd`.

### 7.3 Как изменить память или размер диска

**Память.**

1. Остановить сервис: `make service-stop`.
2. Задать желаемое значение (ГБ) и перезапустить:
   ```bash
   export ANVIL_MEMORY=4
   make service-start
   ```
   Хеш снапшота включает размер памяти, поэтому при смене произойдёт cold boot и снапшот пересоздастся автоматически.

**Диск containerd.**

Диск — preallocated raw image (`containerd-disk.img`) с одним ext4-разделом
внутри. Raw выбран вместо UDIF sparse-образа, чтобы APFS не тратил ресурсы
на выделение блоков за EOF при каждой записи containerd metadata.
Увеличить его in-place нельзя, поэтому безопасный путь — создать новый raw
большего размера и скопировать данные.

Сохранить данные:

```bash
make service-stop
# Создать новый preallocated raw image (32 GB пример)
dd if=/dev/zero of="$HOME/.anvil-vz/containerd-disk-new.img" bs=1m count=32768
limactl start anvil
limactl shell anvil

sudo mkdir -p /tmp/anvil-old /tmp/anvil-new
OLD_DEV=$(sudo losetup -f --show "$HOME/.anvil-vz/containerd-disk.img")
NEW_DEV=$(sudo losetup -f --show "$HOME/.anvil-vz/containerd-disk-new.img")

sudo e2fsck -f "$OLD_DEV"
sudo mkfs.ext4 -F "$NEW_DEV"
sudo mount -t ext4 "$OLD_DEV" /tmp/anvil-old
sudo mount -t ext4 "$NEW_DEV" /tmp/anvil-new
sudo cp -a /tmp/anvil-old/. /tmp/anvil-new/
sudo umount /tmp/anvil-old /tmp/anvil-new
sudo losetup -d "$OLD_DEV" "$NEW_DEV"
exit

mv "$HOME/.anvil-vz/containerd-disk-new.img" "$HOME/.anvil-vz/containerd-disk.img"
limactl stop anvil
make service-start
```

Если данные не нужны, пересоздайте пустой диск и отформатируйте его в ext4:

```bash
make service-stop
# 16 GB preallocated raw image
dd if=/dev/zero of="$HOME/.anvil-vz/containerd-disk.img" bs=1m count=16384
limactl start anvil
limactl shell anvil
DEV=$(sudo losetup -f --show "$HOME/.anvil-vz/containerd-disk.img")
sudo mkfs.ext4 -F "$DEV"
sudo losetup -d "$DEV"
exit
limactl stop anvil
make service-start
```

Если диск не указан, stage2 использует fallback `/mnt/anvil/var-lib` на
virtiofs share — он медленнее, но не требует ручного создания образа.

## 8. Известные компромиссы

- **Snapshot size floor.** Даже с block disk и `drop_caches` снапшот после pull образов ~1 GB из-за активной памяти containerd. Дальнейшее уменьшение возможно только через более агрессивное управление кэшем containerd или уменьшение RAM.
- **Docker API partial.** Реализовано только подмножество Docker API, достаточное для `docker`/`docker compose` повседневного использования. Часть редких endpoint'ов возвращает `404`.
- **Single VM.** Все проекты в одной VM. Если VM падает — останавливаются все проекты. Изоляция — логическая (containerd namespace + CNI bridge), не аппаратная.
- **macOS only.** `Virtualization.framework` доступен только на Apple Silicon macOS.
