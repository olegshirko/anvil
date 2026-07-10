import Foundation
import Virtualization

enum BootCommand {
    static func run(args: [String]) {
        setbuf(stdout, nil)
        guard let cliArgs = parseBootArgs(args) else {
            printUsage()
            exit(1)
        }

        let manager = VMLifecycleManager(args: cliArgs)
        let delegate = BootDelegate(args: cliArgs, manager: manager)
        manager.delegate = delegate

        signal(SIGINT) { _ in
            print("\n[vz-runner] received SIGINT, exiting...")
            exit(0)
        }

        manager.start()
        RunLoop.main.run()
    }

    private class BootDelegate: VMLifecycleManagerDelegate {
        private let args: BootArgs
        private let manager: VMLifecycleManager
        private var server: ControlServer?

        init(args: BootArgs, manager: VMLifecycleManager) {
            self.args = args
            self.manager = manager
        }

        func vmLifecycleManagerDidBecomeReady(_ manager: VMLifecycleManager) {
            guard args.useAgent else { return }
            let server = ControlServer(socketPath: controlSocketPath) { [weak manager] in
                manager?.socketDevice
            }
            server.start()
            self.server = server
            print("[vz-runner] control server ready on \(controlSocketPath)")
        }

        func vmLifecycleManager(_ manager: VMLifecycleManager, didFailWithError error: Error) {
            print("[vz-runner] VM failed: \(error)")
            exit(1)
        }

        func vmLifecycleManagerDidStop(_ manager: VMLifecycleManager) {
            print("\n[vz-runner] VM stopped")
            exit(0)
        }
    }
}
