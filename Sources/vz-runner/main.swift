// M0/M1: boot Linux VM через Virtualization.framework и управлять ей
// через virtio-vsock guest-agent без SSH.
//
// Собирать и запускать ТОЛЬКО на macOS arm64 (13+), с Xcode/Swift toolchain.
// Нужен entitlement com.apple.security.virtualization — см. Makefile.
//
// Использование:
//   vz-runner boot --kernel /path/vmlinuz --initrd /path/initrd [--agent]  (uses rdinit=/myinit)
//   vz-runner status
//   vz-runner exec <cmd>...

import Foundation

func printUsage() {
    print("""
    Usage:
      vz-runner daemon [--kernel <path>] [--initrd <path>] [--share <path>]
      vz-runner boot --kernel <path> --initrd <path> [--agent]
      vz-runner status
      vz-runner exec <command> [args...]

    Daemon options:
      --kernel <path>   Path to the Linux kernel image (default: .download/ubuntu/vmlinuz-raw)
      --initrd <path>   Path to the initramfs/initrd image (default: .download/ubuntu/initramfs-containerd)
      --share <path>    Share host directory via virtiofs
      --idle <seconds>  Seconds to wait before pausing VM (default: 60)

    Boot options:
      --kernel <path>   Path to the Linux kernel image (ARM64 Image format)
      --initrd <path>   Path to the initramfs/initrd image
      --agent           Use rdinit=/myinit (for initramfs with guest-agent)
      --fresh           Force cold boot and recreate snapshot
      --share <path>    Share host directory via virtiofs
      --mount-tag <tag> Virtiofs tag (default: anvil)
      --cpus <n>        Number of CPUs (default: 2)
      --memory <gib>    Memory in GiB (default: 2)
    """)
}

let arguments = Array(CommandLine.arguments.dropFirst())
guard let subcommand = arguments.first else {
    printUsage()
    exit(1)
}

switch subcommand {
case "daemon":
    DaemonCommand.run(args: Array(arguments.dropFirst()))
case "boot":
    BootCommand.run(args: Array(arguments.dropFirst()))
case "status":
    ControlClient.status()
case "exec":
    let cmdArgs = Array(arguments.dropFirst())
    guard !cmdArgs.isEmpty else {
        printUsage()
        exit(1)
    }
    ControlClient.exec(command: cmdArgs)
case "--help", "-h":
    printUsage()
    exit(0)
default:
    printUsage()
    exit(1)
}
