{
  description = "Anvil — container runtimes for macOS and Linux via Lima VMs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/25.05";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = import ./anvil.nix { inherit pkgs; };
        devShells.default = import ./shell.nix { inherit pkgs; };
        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/anvil";
        };
      }
    );
}
