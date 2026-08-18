import Foundation
import Virtualization

/// Virtiofs tag for the host /Users share.
let usersShareTag = "macusers"

/// Host directory shared into the guest at the same absolute path (/Users),
/// so `docker run -v $HOME/...:/path` bind mounts work like on Docker
/// Desktop / Lima. Disable with ANVIL_SHARE_USERS=0.
func usersSharePath() -> String? {
    if ProcessInfo.processInfo.environment["ANVIL_SHARE_USERS"] == "0" {
        return nil
    }
    var isDir: ObjCBool = false
    guard FileManager.default.fileExists(atPath: "/Users", isDirectory: &isDir),
          isDir.boolValue else {
        return nil
    }
    return "/Users"
}

struct BootArgs {
    var kernelPath: String
    var initrdPath: String
    var useAgent: Bool = false
    var fresh: Bool = false
    var sharePath: String?
    var mountTag: String = "anvil"
    var memoryGiB: UInt64 = 2
    var cpuCount: Int = 2
    var consoleOutputPath: String?
    /// Path to a host disk image that will be attached to the VM and used
    /// for /var/lib/containerd. Keeping containerd state on a block device
    /// removes the tmpfs memory pressure and shrinks the VM memory snapshot.
    var containerdDiskPath: String?
    /// Enable verbose logging for the docker proxy and related host-side paths.
    var debug: Bool = false
}

func parseBootArgs(_ raw: [String]) -> BootArgs? {
    var kernel: String?
    var initrd: String?
    var useAgent = false
    var fresh = false
    var sharePath: String?
    var mountTag: String = "anvil"
    var consoleOutputPath: String?
    var containerdDiskPath: String?
    var cpus: Int = 2
    var memory: UInt64 = 2
    var debug = false

    var it = raw.makeIterator()
    while let a = it.next() {
        switch a {
        case "--kernel":
            kernel = it.next()
        case "--initrd":
            initrd = it.next()
        case "--agent":
            useAgent = true
        case "--fresh":
            fresh = true
        case "--share":
            sharePath = it.next()
        case "--mount-tag":
            if let v = it.next() { mountTag = v }
        case "--console":
            consoleOutputPath = it.next()
        case "--containerd-disk":
            containerdDiskPath = it.next()
        case "--cpus":
            if let v = it.next() { cpus = Int(v) ?? cpus }
        case "--memory":
            if let v = it.next() { memory = UInt64(v) ?? memory }
        case "--debug":
            debug = true
        case "--help", "-h":
            return nil
        default:
            break
        }
    }

    guard let k = kernel, let i = initrd else {
        return nil
    }
    return BootArgs(
        kernelPath: k,
        initrdPath: i,
        useAgent: useAgent,
        fresh: fresh,
        sharePath: sharePath,
        mountTag: mountTag,
        memoryGiB: memory,
        cpuCount: cpus,
        consoleOutputPath: consoleOutputPath,
        containerdDiskPath: containerdDiskPath,
        debug: debug
    )
}

func makeConfiguration(
    _ args: BootArgs,
    machineIdentifier: Data? = nil,
    macAddress: String? = nil
) throws -> VZVirtualMachineConfiguration {
    let config = VZVirtualMachineConfiguration()

    let platform = VZGenericPlatformConfiguration()
    if let data = machineIdentifier,
       let identifier = VZGenericMachineIdentifier(dataRepresentation: data) {
        platform.machineIdentifier = identifier
    }
    config.platform = platform

    let bootLoader = VZLinuxBootLoader(kernelURL: URL(fileURLWithPath: args.kernelPath))
    bootLoader.initialRamdiskURL = URL(fileURLWithPath: args.initrdPath)
    var cmdline = "console=hvc0 elevator=none random.trust_cpu=on"
    if args.useAgent {
        cmdline += " rdinit=/myinit"
    }
    bootLoader.commandLine = cmdline
    config.bootLoader = bootLoader

    config.cpuCount = args.cpuCount
    config.memorySize = args.memoryGiB * 1024 * 1024 * 1024

    // Serial port: use stdio for one-shot boot, redirect to a file for daemon mode
    // so snapshot restore isn't tied to the original process's stdin/stdout.
    let serialConfig = VZVirtioConsoleDeviceSerialPortConfiguration()
    if let consolePath = args.consoleOutputPath {
        FileManager.default.createFile(atPath: consolePath, contents: nil, attributes: nil)
        let readHandle = FileHandle(forReadingAtPath: "/dev/null") ?? FileHandle.standardInput
        let writeHandle = FileHandle(forWritingAtPath: consolePath) ?? FileHandle.standardOutput
        // Opened without O_TRUNC: a shorter boot would leave the previous
        // run's tail readable after the new content, mixing runs.
        try? writeHandle.truncate(atOffset: 0)
        serialConfig.attachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: readHandle,
            fileHandleForWriting: writeHandle
        )
    } else {
        serialConfig.attachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: FileHandle.standardInput,
            fileHandleForWriting: FileHandle.standardOutput
        )
    }
    config.serialPorts = [serialConfig]

    // M1: virtio-socket control device.
    let socketConfig = VZVirtioSocketDeviceConfiguration()
    config.socketDevices = [socketConfig]

    // M2: virtiofs shared directories: the anvil state share (/mnt/anvil in
    // the guest) plus the host /Users tree at the same absolute path, so
    // bind mounts of macOS paths work unchanged.
    var sharingDevices: [VZVirtioFileSystemDeviceConfiguration] = []
    if let sharePath = args.sharePath {
        let sharedDirectory = VZSharedDirectory(
            url: URL(fileURLWithPath: sharePath),
            readOnly: false
        )
        let fsConfig = VZVirtioFileSystemDeviceConfiguration(tag: args.mountTag)
        fsConfig.share = VZSingleDirectoryShare(directory: sharedDirectory)
        sharingDevices.append(fsConfig)
    }
    if let usersPath = usersSharePath() {
        let sharedDirectory = VZSharedDirectory(
            url: URL(fileURLWithPath: usersPath),
            readOnly: false
        )
        let fsConfig = VZVirtioFileSystemDeviceConfiguration(tag: usersShareTag)
        fsConfig.share = VZSingleDirectoryShare(directory: sharedDirectory)
        sharingDevices.append(fsConfig)
    }
    config.directorySharingDevices = sharingDevices

    // M5: vzNAT network device. The MAC address must be stable across
    // save/restore, otherwise restore fails with invalid argument.
    let networkAttachment = VZNATNetworkDeviceAttachment()
    let networkDevice = VZVirtioNetworkDeviceConfiguration()
    networkDevice.attachment = networkAttachment
    if let mac = macAddress, let addr = VZMACAddress(string: mac) {
        networkDevice.macAddress = addr
    }
    config.networkDevices = [networkDevice]

    // M8: persistent block device for /var/lib. Images and snapshots live on
    // disk, so the VM needs less RAM and the memory-only snapshot stays small.
    // Use host writeback caching with no guest-fsync synchronization: this makes
    // nerdctl/docker metadata operations fast. Durability is provided by the
    // snapshot save/resume path; an abnormal host shutdown may lose the last
    // metadata changes, which is acceptable for a development VM.
    if let diskPath = args.containerdDiskPath {
        let diskURL = URL(fileURLWithPath: diskPath)
        let attachment = try VZDiskImageStorageDeviceAttachment(
            url: diskURL,
            readOnly: false,
            cachingMode: .cached,
            synchronizationMode: .none
        )
        let blockDevice = VZVirtioBlockDeviceConfiguration(attachment: attachment)
        config.storageDevices = [blockDevice]
    }

    do {
        try config.validate()
    } catch {
        print("[anvil] configuration validation failed: \(error)")
        throw error
    }
    return config
}

class VMDelegate: NSObject, VZVirtualMachineDelegate {
    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        print("\n[anvil] guest stopped")
        exit(0)
    }

    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        print("\n[anvil] VM stopped with error: \(error)")
        exit(1)
    }
}
