# Установка

## Homebrew (рекомендуется)

```sh
brew tap olegshirko/tap
brew install anvil
```

Самая свежая версия:

```sh
brew install --HEAD anvil
```

> **Примечание:** `anvil` пока не в `homebrew/core`. Распространяется через собственный tap (`olegshirko/tap`).

## Nix

```sh
nix-build
```

Или войди в dev-оболочку:

```sh
nix-shell -p go lima qemu
```

## Бинарник

```sh
curl -LO https://github.com/olegshirko/anvil/releases/latest/download/anvil-$(uname)-$(uname -m)
sudo install anvil-$(uname)-$(uname -m) /usr/local/bin/anvil
```

Все сборки — на [странице релизов](https://github.com/olegshirko/anvil/releases).

## Исходники

Требуется [Go](https://golang.org) (>= 1.23).

```sh
git clone https://github.com/olegshirko/anvil.git
cd anvil
make
sudo make install
```

## MacPorts

*Скоро.*
