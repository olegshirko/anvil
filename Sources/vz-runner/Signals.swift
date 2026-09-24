import Foundation

/// Run `handler` on the main queue when `sig` arrives. The default action is
/// ignored so the dispatch source, not the kernel, handles the signal.
private var signalSources: [DispatchSourceSignal] = []

func onSignal(_ sig: Int32, handler: @escaping () -> Void) {
    signal(sig, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: sig, queue: .main)
    source.setEventHandler(handler: handler)
    source.resume()
    signalSources.append(source)
}
