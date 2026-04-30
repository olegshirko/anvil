# Installation

## Homebrew (recommended)

```sh
brew tap olegshirko/tap
brew install anvil
```

Bleeding edge:

```sh
brew install --HEAD anvil
```

> **Note:** `anvil` is not yet in `homebrew/core`. It is distributed via a custom tap (`olegshirko/tap`).

## Nix

```sh
nix-build
```

Or enter a dev shell:

```sh
nix-shell -p go lima qemu
```

## Binary

```sh
curl -LO https://github.com/olegshirko/anvil/releases/latest/download/anvil-$(uname)-$(uname -m)
sudo install anvil-$(uname)-$(uname -m) /usr/local/bin/anvil
```

See the [releases page](https://github.com/olegshirko/anvil/releases) for all builds.

## Source

Requires [Go](https://golang.org) (>= 1.23).

```sh
git clone https://github.com/olegshirko/anvil.git
cd anvil
make
sudo make install
```

## MacPorts

*Coming soon.*
