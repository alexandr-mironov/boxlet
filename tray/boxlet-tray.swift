// boxlet-tray — иконка boxlet в менюбаре macOS.
//
// Показывает состояние движка и расход ресурсов гостевой ОС, даёт
// start/stop/restart и переход в веб-интерфейс. Общается с демоном
// boxletd по его HTTP API (тот же, что у CLI и веб-интерфейса);
// собирается штатным swiftc, без Xcode-проекта и внешних зависимостей.
//
// Запуск: boxlet tray (или напрямую bin/boxlet-tray).

import Cocoa

// MARK: - Конфигурация

let fm = FileManager.default

// Порт control API — из ~/.boxlet/config.json (api_port), дефолт 9770.
func apiPort() -> Int {
    let url = fm.homeDirectoryForCurrentUser.appendingPathComponent(".boxlet/config.json")
    guard let data = try? Data(contentsOf: url),
          let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let port = obj["api_port"] as? Int, port > 0 else { return 9770 }
    return port
}

let apiBase = "http://127.0.0.1:\(apiPort())"
let pidFile = fm.homeDirectoryForCurrentUser.appendingPathComponent(".boxlet/run/boxlet-tray.pid")

// MARK: - Модель состояния

struct EngineStatus {
    var reachable = false
    var running = false
    var state = ""
    var detail = ""
    var backend = ""
    var metrics: String? = nil
}

// MARK: - Приложение

final class TrayApp: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem!
    private var stateItem: NSMenuItem!
    private var metricsItem: NSMenuItem!
    private var startItem: NSMenuItem!
    private var stopItem: NSMenuItem!
    private var restartItem: NSMenuItem!
    private var daemonItem: NSMenuItem!
    private var last = EngineStatus()

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        setIcon(running: false, reachable: false)

        let menu = NSMenu()
        stateItem = menu.addItem(withTitle: "Boxlet: …", action: nil, keyEquivalent: "")
        stateItem.isEnabled = false
        metricsItem = menu.addItem(withTitle: "", action: nil, keyEquivalent: "")
        metricsItem.isEnabled = false
        menu.addItem(.separator())
        startItem = menu.addItem(withTitle: "Старт", action: #selector(doStart), keyEquivalent: "")
        stopItem = menu.addItem(withTitle: "Стоп", action: #selector(doStop), keyEquivalent: "")
        restartItem = menu.addItem(withTitle: "Рестарт", action: #selector(doRestart), keyEquivalent: "")
        menu.addItem(.separator())
        menu.addItem(withTitle: "Открыть веб-интерфейс", action: #selector(openUI), keyEquivalent: "u")
        menu.addItem(.separator())
        daemonItem = menu.addItem(withTitle: "Запустить демон", action: #selector(startDaemon), keyEquivalent: "")
        menu.addItem(withTitle: "Выход", action: #selector(quit), keyEquivalent: "q")
        menu.items.forEach { $0.target = self }
        statusItem.menu = menu

        refresh()
        Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { _ in self.refresh() }
    }

    func applicationWillTerminate(_ notification: Notification) {
        try? fm.removeItem(at: pidFile)
    }

    // MARK: Иконка

    private func setIcon(running: Bool, reachable: Bool) {
        guard let button = statusItem.button else { return }
        let name = running ? "shippingbox.fill" : "shippingbox"
        button.image = NSImage(systemSymbolName: name, accessibilityDescription: "Boxlet")
        // Недоступный демон — полупрозрачная иконка.
        button.alphaValue = reachable ? 1.0 : 0.45
    }

    // MARK: Опрос API

    private func refresh() {
        getJSON("\(apiBase)/api/status") { obj in
            var st = EngineStatus()
            if let obj = obj {
                st.reachable = true
                st.running = obj["running"] as? Bool ?? false
                st.state = obj["state"] as? String ?? "?"
                st.detail = obj["detail"] as? String ?? ""
                st.backend = obj["backend"] as? String ?? ""
            }
            DispatchQueue.main.async { self.apply(st) }
            if st.running {
                getJSON("\(apiBase)/api/metrics") { m in
                    guard let m = m,
                          let load = m["load"] as? String, !load.isEmpty else { return }
                    var s = "load \(load)"
                    if let used = m["mem_used_mib"] as? Int, let total = m["mem_total_mib"] as? Int, total > 0 {
                        s += ", RAM \(used)/\(total) MiB"
                    }
                    DispatchQueue.main.async {
                        self.last.metrics = s
                        self.metricsItem.title = s
                        self.metricsItem.isHidden = false
                    }
                }
            }
        }
    }

    private func apply(_ st: EngineStatus) {
        last = st
        setIcon(running: st.running, reachable: st.reachable)
        if !st.reachable {
            stateItem.title = "Boxlet: демон недоступен"
            metricsItem.isHidden = true
        } else {
            stateItem.title = "Boxlet: \(st.state)" + (st.detail.isEmpty ? "" : " — \(st.detail)")
            metricsItem.isHidden = !st.running
            if !st.running { metricsItem.title = "" }
        }
        startItem.isEnabled = st.reachable && !st.running
        stopItem.isEnabled = st.reachable && st.running
        restartItem.isEnabled = st.reachable && st.running
        daemonItem.isHidden = st.reachable
    }

    // MARK: Действия

    @objc private func doStart()   { post("/api/start") }
    @objc private func doStop()    { post("/api/stop") }

    @objc private func doRestart() {
        let alert = NSAlert()
        alert.messageText = "Перезапустить движок?"
        alert.informativeText = "Контейнеры будут остановлены."
        alert.addButton(withTitle: "Рестарт")
        alert.addButton(withTitle: "Отмена")
        if alert.runModal() == .alertFirstButtonReturn {
            post("/api/restart")
        }
    }

    @objc private func openUI() {
        if let url = URL(string: apiBase) { NSWorkspace.shared.open(url) }
    }

    // Демон не отвечает — запускаем boxletd рядом с нами (тот же bin/).
    @objc private func startDaemon() {
        let dir = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent()
        let boxletd = dir.appendingPathComponent("boxletd").path
        guard fm.isExecutableFile(atPath: boxletd) else { return }
        let logs = fm.homeDirectoryForCurrentUser.appendingPathComponent(".boxlet/logs")
        try? fm.createDirectory(at: logs, withIntermediateDirectories: true)
        let logURL = logs.appendingPathComponent("boxletd.log")
        if !fm.fileExists(atPath: logURL.path) { fm.createFile(atPath: logURL.path, contents: nil) }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: boxletd)
        if let h = try? FileHandle(forWritingTo: logURL) {
            h.seekToEndOfFile()
            p.standardOutput = h
            p.standardError = h
        }
        try? p.run()
    }

    @objc private func quit() { NSApp.terminate(nil) }

    // MARK: HTTP

    private func post(_ path: String) {
        guard let url = URL(string: apiBase + path) else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        URLSession.shared.dataTask(with: req) { _, _, _ in
            DispatchQueue.main.asyncAfter(deadline: .now() + 1) { self.refresh() }
        }.resume()
    }
}

// GET JSON с коротким таймаутом; nil = недоступно.
func getJSON(_ urlString: String, done: @escaping ([String: Any]?) -> Void) {
    guard let url = URL(string: urlString) else { done(nil); return }
    var req = URLRequest(url: url)
    req.timeoutInterval = 2
    URLSession.shared.dataTask(with: req) { data, resp, _ in
        guard let data = data,
              let http = resp as? HTTPURLResponse, http.statusCode == 200,
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        else { done(nil); return }
        done(obj)
    }.resume()
}

// MARK: - main: синглтон по pid-файлу и запуск

// Не даём запустить вторую копию трея (две иконки в менюбаре).
if let data = try? Data(contentsOf: pidFile),
   let pid = Int32(String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""),
   kill(pid, 0) == 0 {
    // уже запущен — тихо выходим
    exit(0)
}
try? fm.createDirectory(at: pidFile.deletingLastPathComponent(), withIntermediateDirectories: true)
try? "\(ProcessInfo.processInfo.processIdentifier)".write(to: pidFile, atomically: true, encoding: .utf8)

let app = NSApplication.shared
app.setActivationPolicy(.accessory) // без иконки в доке, только менюбар
let delegate = TrayApp()
app.delegate = delegate
app.run()
