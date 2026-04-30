<!--
  Anvil README (Russian)
-->


<h1 align="center">Anvil</h1>
<p align="center">
  <b>Сплав рантаймов контейнеров на macOS и Linux.</b>
</p>

<p align="center">
  <a href="https://github.com/olegshirko/anvil/actions/workflows/go.yml"><img src="https://img.shields.io/github/actions/workflow/status/olegshirko/anvil/go.yml?branch=main&label=build&style=flat-square" alt="Build"></a>
  <a href="https://github.com/olegshirko/anvil/releases/latest"><img src="https://img.shields.io/github/v/release/olegshirko/anvil?sort=semver&style=flat-square&color=orange" alt="Release"></a>
  <a href="https://golang.org/doc/go1.25"><img src="https://img.shields.io/badge/go-1.25+-00ADD8?style=flat-square&logo=go" alt="Go"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License"></a>
  <a href="https://brew.sh"><img src="https://img.shields.io/badge/brew-olegshirko%2Ftap-FFBB00?style=flat-square&logo=homebrew" alt="Homebrew"></a>
</p>

---

## 

Docker Desktop - медленный и безжалостно садит батарею.  
**Anvil** - легковесный, твой Air не расплавится (надеюсь).

- **Чистая Архитектура** - рассчитан на долгую жизнь.
- **Быстрый старт** - холодный старт ~25 с.
- **Сплав рантаймов** - Docker, Containerd или Incus.
- **Зеркальный резерв** - когда Docker Hub недоступен, Anvil стянет образ из релизов githab.


---

## Установка

```sh
# 1. 
brew tap olegshirko/tap
brew install anvil

# 2. 
anvil start

# 3. 
docker run hello-world
```

---

## Что умеет

|  | Возможности                                                                     |
|---|---------------------------------------------------------------------------------|
| 🐳 | **Docker** - полноценный Docker CE внутри Lima VM, доступный нативно на macOS   |
| 🦭 | **Containerd** - лёгкая альтернатива с `nerdctl` и Buildkit                     |
| 🧊 | **Incus** - системные контейнеры и виртуальные машины (VM на Apple Silicon M3+) |
| ☸️ | **Kubernetes** - опциональный K3s внутри VM, без мусора на хосте                |
| 🍎 | **Apple Silicon и Intel** - нативный `vz` на macOS 13+, fallback на QEMU        |
| 🐧 | **Linux** - Linux, без комментариев                                             |
| 🪞 | **Docker Mirror** - резерв для изолированных сред, когда Docker Hub недоступен  |
| ⚡ | **Параллельные загрузки** - многопоточные HTTP Range-загрузки больших образов   |
| 🏗️ | **Clean Architecture** - ...                                                    |
| 🔧 | **Мульти-инстанс** - профили позволяют держать Docker *и* Containerd рядом      |
| 📦 | **Мульти-версионные образы** - Ubuntu 24.04, 26.04 и далее                      |

---

### Docker *(по умолчанию)*

```sh
anvil start
```

Клиент `docker` на macOS работает сразу.

### Containerd

```sh
anvil start --runtime containerd
anvil nerdctl install   # опционально: добавить nerdctl в $PATH
```

### Kubernetes

```sh
anvil start --kubernetes
```

K3s поднимается внутри VM. Образы, собранные или скачанные через Docker, автоматически доступны Kubernetes.

### Incus

```sh
anvil start --runtime incus
```

Системные контейнеры и виртуальные машины. Режим VM требует Apple Silicon M3 или новее.

### Без рантайма

```sh
anvil start --runtime none
```

Просто Lima VM - без контейнерного рантайма. Иногда бывает нужно.

---

## Зеркало

Когда нет докер регистри

```sh
# Посмотри, что доступно
anvil images list

# Проверь перед загрузкой
anvil images check --docker postgres:15.8

# Загрузь из зеркала напрямую
anvil images load --docker postgres:15.8
```

`anvil compose up -d` автоматически переключается на зеркало, если `docker pull` падает - без ручного вмешательства.

Если не получилось - запроси недостающие образы через GitHub issue:

```sh
anvil images request --docker postgres:15.8
```

---

### Дефолтные настройки

По умолчанию: 2 CPU, 2 ГиБ памяти, 100 ГиБ хранилища.

```sh
# Меньший молот
anvil start --cpu 1 --memory 2 --disk 10

# Больший молот
anvil stop
anvil start --cpu 4 --memory 8

# Сплав Rosetta 2 (macOS 13+ на Apple Silicon)
anvil start --vm-type=vz --vz-rosetta
```

Конфиг можно поправить из консоли:

```sh
anvil start --edit
```

### Диагностика

```sh
anvil doctor
```

### Проверки

```sh
anvil status
anvil health
```

---

## Установка

| Способ | Команда |
|---|---|
| **Homebrew** (рекомендуется) | `brew tap olegshirko/tap && brew install anvil` |
| **Homebrew HEAD** | `brew install --HEAD anvil` |
| **Nix** | `nix-build` |
| **Бинарник** | См. [Releases](https://github.com/olegshirko/anvil/releases) |
| **Из исходников** | `git clone … && make && sudo make install` |

**Поддерживается:** macOS (Intel или Apple Silicon) или Linux, а также `limactl` и QEMU (или `vz` на macOS 13+).

---

## Лицензия

MIT - см. [LICENSE](LICENSE).

---

<p align="center">
  <i>Каждый великий инструмент начинается с наковальни.</i>
</p>
