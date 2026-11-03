// boxlet-vm — helper-процесс бэкенда darwin-vz: поднимает Linux VM через
// Virtualization.framework и держит её, пока не получит SIGTERM.
//
// Управляется boxletd как дочерний процесс. Параметры — аргументы
// командной строки:
//
//	boxlet-vm --kernel <path> --initrd <path> --cmdline "..." \
//	    --disk <path>[:ro] ... --share <tag>=<path> ... \
//	    --cpus N --memory-mib N --mac <addr> --serial <path> \
//	    [--rosetta]
//
// Сеть: NAT (vmnet). IP гостя boxletd узнаёт по MAC из
// /var/db/dhcpd_leases. Диски — raw-образы (VZ не читает qcow2).
import Foundation
import Virtualization

struct HelperConfig {
    var kernel = ""
    var initrd = ""
    var cmdline = ""
    var disks: [(url: URL, readOnly: Bool)] = []
    var shares: [(tag: String, path: String)] = []
    var cpus = 4
    var memoryMiB = 4096
    var mac = ""
    var serial = ""
    var rosetta = false
}

func parseArgs() -> HelperConfig {
    var cfg = HelperConfig()
    var args = Array(CommandLine.arguments.dropFirst())
    while !args.isEmpty {
        let arg = args.removeFirst()
        let value: () -> String = {
            guard !args.isEmpty else { fatalError("аргумент \(arg) требует значение") }
            return args.removeFirst()
        }
        switch arg {
        case "--kernel": cfg.kernel = value()
        case "--initrd": cfg.initrd = value()
        case "--cmdline": cfg.cmdline = value()
        case "--disk":
            let spec = value()
            if spec.hasSuffix(":ro") {
                cfg.disks.append((URL(fileURLWithPath: String(spec.dropLast(3))), true))
            } else {
                cfg.disks.append((URL(fileURLWithPath: spec), false))
            }
        case "--share":
            let spec = value()
            guard let eq = spec.firstIndex(of: "=") else {
                fatalError("--share ожидает формат tag=path, получено: \(spec)")
            }
            cfg.shares.append((String(spec[..<eq]), String(spec[spec.index(after: eq)...])))
        case "--cpus": cfg.cpus = Int(value()) ?? 4
        case "--memory-mib": cfg.memoryMiB = Int(value()) ?? 4096
        case "--mac": cfg.mac = value()
        case "--serial": cfg.serial = value()
        case "--rosetta": cfg.rosetta = true
        default: fatalError("неизвестный аргумент: \(arg)")
        }
    }
    return cfg
}

func buildVMConfiguration(_ cfg: HelperConfig) throws -> VZVirtualMachineConfiguration {
    let bootLoader = VZLinuxBootLoader(kernelURL: URL(fileURLWithPath: cfg.kernel))
    bootLoader.commandLine = cfg.cmdline
    bootLoader.initialRamdiskURL = URL(fileURLWithPath: cfg.initrd)

    let vmConfig = VZVirtualMachineConfiguration()
    vmConfig.bootLoader = bootLoader
    vmConfig.cpuCount = cfg.cpus
    vmConfig.memorySize = UInt64(cfg.memoryMiB) * 1024 * 1024
    vmConfig.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    vmConfig.socketDevices = [VZVirtioSocketDeviceConfiguration()]

    for disk in cfg.disks {
        let attachment = try VZDiskImageStorageDeviceAttachment(url: disk.url, readOnly: disk.readOnly)
        vmConfig.storageDevices.append(VZVirtioBlockDeviceConfiguration(attachment: attachment))
    }

    let network = VZVirtioNetworkDeviceConfiguration()
    network.attachment = VZNATNetworkDeviceAttachment()
    if !cfg.mac.isEmpty {
        guard let mac = VZMACAddress(string: cfg.mac) else {
            throw NSError(domain: "boxlet-vm", code: 1,
                          userInfo: [NSLocalizedDescriptionKey: "некорректный MAC: \(cfg.mac)"])
        }
        network.macAddress = mac
    }
    vmConfig.networkDevices = [network]

    // Серийная консоль → лог-файл (диагностика загрузки гостевой ОС).
    if !cfg.serial.isEmpty {
        if !FileManager.default.fileExists(atPath: cfg.serial) {
            FileManager.default.createFile(atPath: cfg.serial, contents: nil)
        }
        let handle = try FileHandle(forWritingTo: URL(fileURLWithPath: cfg.serial))
        let serialPort = VZVirtioConsoleDeviceSerialPortConfiguration()
        serialPort.attachment = VZFileHandleSerialPortAttachment(
            fileHandleForReading: nil, fileHandleForWriting: handle)
        vmConfig.serialPorts = [serialPort]
    }

    // virtiofs-шары каталогов хоста (bind-mount в контейнеры).
    for share in cfg.shares {
        let dir = VZSharedDirectory(url: URL(fileURLWithPath: share.path), readOnly: false)
        let device = VZVirtioFileSystemDeviceConfiguration(tag: share.tag)
        device.share = VZSingleDirectoryShare(directory: dir)
        vmConfig.directorySharingDevices.append(device)
    }

    // Rosetta для запуска linux/amd64 образов на Apple Silicon.
    if cfg.rosetta {
        let device = VZVirtioFileSystemDeviceConfiguration(tag: "rosetta")
        device.share = try VZLinuxRosettaDirectoryShare()
        vmConfig.directorySharingDevices.append(device)
    }

    try vmConfig.validate()
    return vmConfig
}

final class VMDelegate: NSObject, VZVirtualMachineDelegate {
    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        FileHandle.standardError.write("boxlet-vm: гостевая ОС остановлена\n".data(using: .utf8)!)
        exit(0)
    }
    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        FileHandle.standardError.write("boxlet-vm: VM остановлена с ошибкой: \(error.localizedDescription)\n".data(using: .utf8)!)
        exit(1)
    }
}

let cfg = parseArgs()

// Создаём raw-диски, которых ещё нет (разреженные на APFS).
for disk in cfg.disks where !disk.readOnly {
    let path = disk.url.path
    if !FileManager.default.fileExists(atPath: path) {
        FileManager.default.createFile(atPath: path, contents: nil)
    }
}

let vmConfig: VZVirtualMachineConfiguration
do {
    vmConfig = try buildVMConfiguration(cfg)
} catch {
    FileHandle.standardError.write("boxlet-vm: конфигурация VM: \(error.localizedDescription)\n".data(using: .utf8)!)
    exit(1)
}

let vm = VZVirtualMachine(configuration: vmConfig)
let delegate = VMDelegate()
vm.delegate = delegate

// SIGTERM → штатная остановка VM.
let termSource = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
signal(SIGTERM, SIG_IGN)
termSource.setEventHandler {
    if vm.canRequestStop {
        try? vm.requestStop()
    } else {
        vm.stop { _ in exit(0) }
    }
}
termSource.resume()

vm.start { result in
    switch result {
    case .success:
        FileHandle.standardError.write("boxlet-vm: VM запущена\n".data(using: .utf8)!)
    case .failure(let error):
        FileHandle.standardError.write("boxlet-vm: запуск VM: \(error.localizedDescription)\n".data(using: .utf8)!)
        exit(1)
    }
}

dispatchMain()
