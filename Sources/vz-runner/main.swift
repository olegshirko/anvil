// M0/M1: boot a Linux VM with Virtualization.framework and control it via
// virtio-vsock guest-agent without SSH.
//
// Build and run ONLY on macOS arm64 (13+) with the Xcode/Swift toolchain.
// Requires the com.apple.security.virtualization entitlement — see Makefile.
//
// Usage:
//   anvil boot --kernel /path/vmlinuz --initrd /path/initrd [--agent]
//                  (uses rdinit=/myinit)
//   anvil status
//   anvil exec <cmd>...

import Foundation

// If this process was spawned by the daemon for a task that must not outlive
// the daemon (e.g. containerd cache sync), exit immediately when the parent
// disappears. This prevents orphan `anvil exec` processes after `kill -9`.
if ProcessInfo.processInfo.environment["ANVIL_EXIT_ON_PARENT_DEATH"] == "1" {
    DispatchQueue.global().async {
        while true {
            Thread.sleep(forTimeInterval: 1.0)
            if getppid() == 1 {
                exit(1)
            }
        }
    }
}

func printUsage() {
    print("""
    Usage:
      anvil start [--kernel <path>] [--initrd <path>] [--share <path>]
      anvil stop
      anvil boot --kernel <path> --initrd <path> [--agent]
      anvil status
      anvil exec <command> [args...]
      anvil docker-socket-path

    Start options:
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
case "start", "daemon":
    DaemonCommand.run(args: Array(arguments.dropFirst()))
case "stop":
    let pidFile = stateDir.appendingPathComponent("daemon.pid")
    guard let data = try? Data(contentsOf: pidFile),
          let s = String(data: data, encoding: .utf8),
          let pid = Int32(s.trimmingCharacters(in: .whitespacesAndNewlines)) else {
        print("[anvil] daemon is not running (no pid file)")
        exit(1)
    }
    guard kill(pid, SIGTERM) == 0 else {
        print("[anvil] failed to stop daemon (pid \(pid)): \(String(cString: strerror(errno)))")
        exit(1)
    }
    print("[anvil] daemon stopped")
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
case "docker-socket-path":
    print(dockerSocketPath)
    exit(0)
case "--help", "-h":
    printUsage()
    exit(0)
default:
    printUsage()
    exit(1)
}
