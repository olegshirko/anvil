import Foundation
import Virtualization

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
}

func parseBootArgs(_ raw: [String]) -> BootArgs? {
    var kernel: String?
    var initrd: String?
    var useAgent = false
    var fresh = false
    var sharePath: String?
    var mountTag: String = "anvil"
    var consoleOutputPath: String?
    var cpus: Int = 2
    var memory: UInt64 = 2

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
        case "--cpus":
            if let v = it.next() { cpus = Int(v) ?? cpus }
        case "--memory":
            if let v = it.next() { memory = UInt64(v) ?? memory }
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
        consoleOutputPath: consoleOutputPath
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
    var cmdline = "console=hvc0"
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

    // M2: virtiofs shared directory.
    if let sharePath = args.sharePath {
        let sharedDirectory = VZSharedDirectory(
            url: URL(fileURLWithPath: sharePath),
            readOnly: false
        )
        let fsConfig = VZVirtioFileSystemDeviceConfiguration(tag: args.mountTag)
        fsConfig.share = VZSingleDirectoryShare(directory: sharedDirectory)
        config.directorySharingDevices = [fsConfig]
    }

    // M5: vzNAT network device. The MAC address must be stable across
    // save/restore, otherwise restore fails with invalid argument.
    let networkAttachment = VZNATNetworkDeviceAttachment()
    let networkDevice = VZVirtioNetworkDeviceConfiguration()
    networkDevice.attachment = networkAttachment
    if let mac = macAddress, let addr = VZMACAddress(string: mac) {
        networkDevice.macAddress = addr
    }
    config.networkDevices = [networkDevice]

    do {
        try config.validate()
    } catch {
        print("[vz-runner] configuration validation failed: \(error)")
        throw error
    }
    return config
}

class VMDelegate: NSObject, VZVirtualMachineDelegate {
    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        print("\n[vz-runner] guest stopped")
        exit(0)
    }

    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        print("\n[vz-runner] VM stopped with error: \(error)")
        exit(1)
    }
}
