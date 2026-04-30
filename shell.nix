{ pkgs ? import <nixpkgs> { } }:

pkgs.mkShell {
  packages = with pkgs; [
    go_1_23
    gotools
    gopls
    git
    lima
    qemu
  ];

  shellHook = ''
    echo "Anvil development environment"
    echo "Go version: $(go version)"

    BUILD_TARGET="$PWD/$(make print-binary-name)"
    if [ -f "$BUILD_TARGET" ]; then
        alias anvil="$BUILD_TARGET"
    else
        echo "Run 'make' to compile the anvil binary."
    fi
  '';
}
