import Foundation

// SupervisorSession 负责与 QCode Web Supervisor 的进程协作：
//
// 1. 先尝试“收养”已有 Supervisor：扫描默认 dataDir（~/.qcode/v1）下的
//    leases/interactive-*.lock，跳过 1 个保留字节解析 JSON 元数据，
//    用 /healthz 探活（与 internal/host/web/launcher.go 的复用语义一致）。
// 2. 收养失败则拉起 .app 内嵌的 qcode-runtime（--no-open，默认端口 6732），
//    解析 stdout 的就绪行，轮询 healthz。
// 3. 若拉起失败但 6732 已有合法 QCode Supervisor（例如 CLI 用了自定义
//    --data-dir 启动），兜底收养该进程。
// 4. 退出时只对自己拉起的进程发 SIGINT 并等待 drain；收养的进程留给原属主。
//
// 实现约束：所有可变状态只在主线程读写。管道读取用 FileHandle
// readabilityHandler，探活用 URLSession 异步回调，全部收敛回主线程，
// 不使用自定义串行队列（避免队列楔死后关停流程整体失效）。
final class SupervisorSession {
    struct Endpoints {
        var baseURL: URL
        var capabilityToken: String?

        init(baseURL: URL, capabilityToken: String? = nil) {
            self.baseURL = baseURL
            self.capabilityToken = capabilityToken
        }
    }

    private struct LeaseMetadata: Decodable {
        let pid: Int
        let publicURL: String?
        let capabilityToken: String?

        enum CodingKeys: String, CodingKey {
            case pid
            case publicURL = "public_url"
            case capabilityToken = "capability_token"
        }
    }

    private struct HealthStatus: Decodable {
        let status: String
    }

    private let urlSession: URLSession
    private var process: Process?
    private var stdoutBuffer = Data()
    private(set) var endpoints: Endpoints?
    private(set) var spawnOwned = false
    private var terminating = false
    private var probeGeneration = 0
    private var sawReadyLine = false

    /// Supervisor 就绪（ready 或 setup_required 都会打开页面）。
    var onReady: ((Endpoints) -> Void)?
    var onFailure: ((String) -> Void)?
    /// 本壳拉起的 Supervisor 意外退出（非关停请求）时触发，调用方决定是否重启。
    var onUnexpectedExit: (() -> Void)?

    init() {
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 2
        config.timeoutIntervalForResource = 5
        urlSession = URLSession(configuration: config)
    }

    static func log(_ message: String) {
        let stamp = ISO8601DateFormatter().string(from: Date())
        let line = "[desktop \(stamp)] \(message)\n"
        FileHandle.standardError.write(Data(line.utf8))
        if let path = ProcessInfo.processInfo.environment["QCODE_DESKTOP_LOG"] {
            if !FileManager.default.fileExists(atPath: path) {
                FileManager.default.createFile(atPath: path, contents: nil)
            }
            if let handle = FileHandle(forWritingAtPath: path) {
                defer { try? handle.close() }
                _ = try? handle.seekToEnd()
                handle.write(Data(line.utf8))
            }
        }
    }

    // MARK: - 启动

    func start() {
        precondition(Thread.isMainThread)
        probeGeneration += 1
        adoptFromLeases()
    }

    // MARK: - 收养已有 Supervisor

    private func adoptFromLeases() {
        let leaseDir = defaultDataDir().appendingPathComponent("leases", isDirectory: true)
        let candidates = (try? FileManager.default.contentsOfDirectory(
            at: leaseDir,
            includingPropertiesForKeys: nil
        ))?.filter { $0.lastPathComponent.hasPrefix("interactive-") && $0.pathExtension == "lock" } ?? []

        // 逐个探测 lease 指向的 Supervisor；收集结果后回到主线程裁决。
        var pending = candidates.count
        var winner: Endpoints?
        func settle() {
            if let winner {
                finishReady(winner, spawnOwned: false)
            } else {
                spawnOwnedProcess()
            }
        }
        guard pending > 0 else {
            spawnOwnedProcess()
            return
        }
        for file in candidates {
            guard let metadata = readLeaseMetadata(at: file),
                  let rawURL = metadata.publicURL,
                  let url = URL(string: rawURL),
                  url.scheme == "http",
                  url.host == "127.0.0.1"
            else {
                pending -= 1
                continue
            }
            probeHealthz(url) { status in
                DispatchQueue.main.async {
                    if let status, status != "draining", winner == nil {
                        winner = Endpoints(baseURL: url, capabilityToken: metadata.capabilityToken)
                    }
                    pending -= 1
                    if pending == 0 { settle() }
                }
            }
        }
        if pending == 0 { settle() }
    }

    private func readLeaseMetadata(at url: URL) -> LeaseMetadata? {
        guard let data = try? Data(contentsOf: url), data.count > 1 else { return nil }
        // lease 文件第 1 个字节是保留字节，其后为单行 JSON。
        return try? JSONDecoder().decode(LeaseMetadata.self, from: data.dropFirst())
    }

    private func probeHealthz(_ base: URL, completion: @escaping (String?) -> Void) {
        let url = base.appendingPathComponent("healthz")
        urlSession.dataTask(with: url) { data, response, _ in
            guard let http = response as? HTTPURLResponse,
                  let data,
                  let status = try? JSONDecoder().decode(HealthStatus.self, from: data).status
            else {
                completion(nil)
                return
            }
            guard http.statusCode == 200 || status == "initializing" || status == "draining" else {
                completion(nil)
                return
            }
            completion(status)
        }.resume()
    }

    // MARK: - 拉起内嵌二进制

    private func embeddedBinaryURL() -> URL? {
        // .app 布局：Contents/MacOS/QCode（壳）与 Contents/MacOS/qcode-runtime（Runtime）。
        // 注意 macOS 默认 APFS 卷大小写不敏感，两个名字不能只差大小写。
        let dir = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS", isDirectory: true)
        let candidate = dir.appendingPathComponent("qcode-runtime")
        if FileManager.default.isExecutableFile(atPath: candidate.path) { return candidate }
        // 开发模式：允许用环境变量指向仓库构建产物。
        if let override = ProcessInfo.processInfo.environment["QCODE_DESKTOP_BINARY"] {
            let url = URL(fileURLWithPath: override)
            if FileManager.default.isExecutableFile(atPath: url.path) { return url }
        }
        Self.log("embedded binary missing at \(candidate.path)")
        return nil
    }

    private func spawnOwnedProcess() {
        guard let binary = embeddedBinaryURL() else {
            fail("未找到内嵌的 qcode Runtime 二进制（Contents/MacOS/qcode-runtime）。")
            return
        }

        let process = Process()
        process.executableURL = binary
        process.arguments = ["--no-open"]
        process.standardInput = FileHandle.nullDevice
        process.environment = ProcessInfo.processInfo.environment

        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe

        process.terminationHandler = { [weak self] terminated in
            DispatchQueue.main.async {
                self?.handleTermination(status: terminated.terminationStatus)
            }
        }

        do {
            try process.run()
        } catch {
            Self.log("spawn failed: \(error)")
            // 拉起失败：兜底探测默认端口，可能已被其他 dataDir 的 Supervisor 占用。
            fallbackAdoptDefaultPort()
            return
        }
        self.process = process
        spawnOwned = true
        Self.log("spawned runtime pid \(process.processIdentifier)")

        stdoutPipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let chunk = handle.availableData
            DispatchQueue.main.async {
                guard let self else { return }
                if chunk.isEmpty {
                    handle.readabilityHandler = nil
                    return
                }
                self.handleSupervisorOutput(chunk)
            }
        }
        stderrPipe.fileHandleForReading.readabilityHandler = { handle in
            let chunk = handle.availableData
            if chunk.isEmpty {
                handle.readabilityHandler = nil
                return
            }
            if let text = String(data: chunk, encoding: .utf8) {
                FileHandle.standardError.write(Data("[qcode] \(text)\n".utf8))
            }
        }
    }

    private func handleSupervisorOutput(_ chunk: Data) {
        stdoutBuffer.append(chunk)
        while let newlineIndex = stdoutBuffer.firstIndex(of: UInt8(ascii: "\n")) {
            let lineData = stdoutBuffer[stdoutBuffer.startIndex..<newlineIndex]
            stdoutBuffer.removeSubrange(stdoutBuffer.startIndex...newlineIndex)
            guard let line = String(data: Data(lineData), encoding: .utf8) else { continue }
            if line.hasPrefix("QCode Runtime Ready: ") ||
                line.hasPrefix("QCode Setup Ready: ") {
                // Runtime Ready 在全部已注册 Workspace 激活完成之后打印，
                // 是比 healthz ready 更强的就绪信号。
                sawReadyLine = true
            }
            if let url = Self.listenURL(fromLine: line) {
                Self.log("supervisor listening at \(url)")
                waitForReady(base: url, generation: probeGeneration, attempt: 0)
            }
        }
    }

    private static func listenURL(fromLine line: String) -> URL? {
        for prefix in ["QCode Web Listening: ", "QCode Runtime Ready: ", "QCode Setup Ready: "] {
            if let range = line.range(of: prefix) {
                return URL(string: String(line[range.upperBound...]).trimmingCharacters(in: .whitespacesAndNewlines))
            }
        }
        return nil
    }

    private func fallbackAdoptDefaultPort() {
        guard let url = URL(string: "http://127.0.0.1:6732/") else { return }
        probeHealthz(url) { [weak self] status in
            DispatchQueue.main.async {
                guard let self else { return }
                if let status, status == "ready" || status == "setup_required" {
                    self.finishReady(Endpoints(baseURL: url), spawnOwned: false)
                } else {
                    self.fail("qcode Runtime 启动失败，且默认端口 127.0.0.1:6732 上没有可用的 QCode 服务。")
                }
            }
        }
    }

    // MARK: - 就绪等待

    private func waitForReady(base: URL, generation: Int, attempt: Int) {
        guard generation == probeGeneration else { return }
        probeHealthz(base) { [weak self] status in
            DispatchQueue.main.async {
                guard let self, generation == self.probeGeneration else { return }
                switch status {
                case "ready", "setup_required":
                    // 自己拉起的 Runtime：healthz ready 只表示首个 Workspace
                    // 激活完成，多 Workspace 仍在逐个激活；等 Ready 行再开窗，
                    // 与前端 awaitWorkspaceReady 双保险。
                    if self.spawnOwned && !self.sawReadyLine {
                        if attempt >= 240 {
                            self.finishReady(Endpoints(baseURL: base), spawnOwned: self.spawnOwned)
                            return
                        }
                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) {
                            self.waitForReady(base: base, generation: generation, attempt: attempt + 1)
                        }
                        return
                    }
                    self.finishReady(Endpoints(baseURL: base), spawnOwned: self.spawnOwned)
                case "initializing", nil:
                    // 启动中或暂时不可达：每 250ms 重试，60s 超时。
                    if attempt >= 240 {
                        self.fail("等待 qcode Runtime 就绪超时（\(base)）。")
                        return
                    }
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) {
                        self.waitForReady(base: base, generation: generation, attempt: attempt + 1)
                    }
                case "draining":
                    self.fail("qcode Runtime 正在关闭中，请稍后重试。")
                default:
                    self.fail("qcode Runtime 返回了未知状态：\(status ?? "nil")")
                }
            }
        }
    }

    private func finishReady(_ endpoints: Endpoints, spawnOwned: Bool) {
        guard self.endpoints == nil else { return }
        self.endpoints = endpoints
        self.spawnOwned = spawnOwned
        Self.log("ready spawnOwned=\(spawnOwned) url=\(endpoints.baseURL)")
        onReady?(endpoints)
    }

    private func fail(_ message: String) {
        Self.log("failure: \(message)")
        onFailure?(message)
    }

    // MARK: - 进程退出处理

    private func handleTermination(status: Int32) {
        let hadEndpoints = endpoints != nil
        process = nil
        endpoints = nil
        if terminating || !spawnOwned {
            return
        }
        if !hadEndpoints {
            Self.log("runtime exited before becoming ready (status \(status))")
            fail("qcode Runtime 启动后立即退出（状态 \(status)），请查看日志后重试。")
            return
        }
        Self.log("runtime exited unexpectedly (status \(status))")
        onUnexpectedExit?()
    }

    // MARK: - 关停

    /// 退出流程：只对自己拉起的 Supervisor 发 SIGINT（Runtime 只捕获 SIGINT，
    /// SIGTERM 会直接杀死进程），并等待 drain（最多约 35s）。收养的进程不动。
    func shutdown(completion: @escaping () -> Void) {
        precondition(Thread.isMainThread)
        Self.log("shutdown: begin, owned=\(spawnOwned), process running=\(process?.isRunning ?? false)")
        terminating = true
        guard let process, process.isRunning else {
            Self.log("shutdown: nothing to stop, replying immediately")
            completion()
            return
        }
        let pid = process.processIdentifier
        Self.log("shutdown: SIGINT runtime pid \(pid)")
        kill(pid, SIGINT)
        pollProcessExit(pid, deadline: Date().addingTimeInterval(35), completion: completion)
    }

    private func pollProcessExit(_ pid: pid_t, deadline: Date, completion: @escaping () -> Void) {
        // kill(pid, 0) 探测进程存活：成功或 EPERM 都表示进程还在。
        let result = kill(pid, 0)
        if result == 0 || errno == EPERM {
            if Date() >= deadline {
                // drain 超时：升级为 SIGKILL，避免卡住 App 退出。
                Self.log("shutdown: deadline hit, SIGKILL runtime pid \(pid)")
                kill(pid, SIGKILL)
                completion()
                return
            }
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { [weak self] in
                self?.pollProcessExit(pid, deadline: deadline, completion: completion)
            }
            return
        }
        Self.log("shutdown: runtime exited, drain complete")
        completion()
    }
}

private func defaultDataDir() -> URL {
    let home = FileManager.default.homeDirectoryForCurrentUser
    return home.appendingPathComponent(".qcode/v1", isDirectory: true)
}
