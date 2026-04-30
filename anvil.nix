{ pkgs ? import <nixpkgs> }:

with pkgs;

buildGoModule {
  name = "anvil";
  pname = "anvil";
  src = ./.;
  nativeBuildInputs = [ installShellFiles makeWrapper git ];
  vendorHash = "sha256-ZwgzKCOEhgKK2LNRLjnWP6qHI4f6OGORvt3CREJf55I=";
  CGO_ENABLED = 1;

  subPackages = [ "cmd/anvil" ];

  # `nix-build` has .git folder but `nix build` does not, this caters for both cases
  preConfigure = ''
    export VERSION="$(git describe --tags --always || echo nix-build-at-"$(date +%s)")"
    export REVISION="$(git rev-parse HEAD || echo nix-unknown)"
    ldflags="-X anvil/internal/usecase.appVersion=$VERSION
              -X anvil/internal/usecase.revision=$REVISION"
  '';

  postInstall = ''
    wrapProgram $out/bin/anvil \
      --prefix PATH : ${lib.makeBinPath [ qemu lima ]}
    installShellCompletion --cmd anvil \
      --bash <($out/bin/anvil completion bash) \
      --fish <($out/bin/anvil completion fish) \
      --zsh <($out/bin/anvil completion zsh)
  '';
}

