import AppKit
import Darwin

final class AppDelegate: NSObject, NSApplicationDelegate {
    private static let activateNotificationName = "QCodeDesktopActivate"

    private var session: SupervisorSession?
    private var window: WebWindow?
    private var singleInstanceFD: Int32 = -1
    private var terminating = false

    func applicationDidFinishLaunching(_ notification: Notification) {
        guard acquireSingleInstance() else {
            // 已有实例在运行：通知它前置窗口，然后自己退出。
            // 不走 NSApp.terminate：在 didFinishLaunching 内触发 terminate
            // 存在不退出的时序怪癖；直接 exit 是确定性的。
            DistributedNotificationCenter.default().postNotificationName(
                Notification.Name(Self.activateNotificationName),
                object: nil,
                userInfo: nil,
                deliverImmediately: true
            )
            exit(0)
        }
        DistributedNotificationCenter.default().addObserver(
            self,
            selector: #selector(activateExistingWindow),
            name: Notification.Name(Self.activateNotificationName),
            object: nil
        )

        buildMenu()
        startSession()
    }

    // MARK: - 单实例

    // 用 App Support 下的 flock 保证只有一个壳实例；持有描述符即持锁。
    // 必须带 O_CLOEXEC：否则 Runtime 子进程继承该描述符，壳退出后
    // 孤儿 Runtime 仍持有锁，导致后续桌面 App 启动被误判为重复实例。
    private func acquireSingleInstance() -> Bool {
        let support = FileManager.default.urls(
            for: .applicationSupportDirectory,
            in: .userDomainMask
        ).first?.appendingPathComponent("QCode", isDirectory: true)
        guard let support else { return true }
        try? FileManager.default.createDirectory(at: support, withIntermediateDirectories: true)
        let lockPath = support.appendingPathComponent("app.lock").path
        let fd = open(lockPath, O_CREAT | O_RDWR | O_CLOEXEC, 0o600)
        guard fd >= 0 else { return true }
        guard flock(fd, LOCK_EX | LOCK_NB) == 0 else {
            close(fd)
            return false
        }
        singleInstanceFD = fd
        return true
    }

    @objc private func activateExistingWindow() {
        DispatchQueue.main.async {
            NSApp.activate(ignoringOtherApps: true)
            self.window?.makeKeyAndOrderFront(nil)
        }
    }

    // MARK: - Supervisor 会话

    private func startSession() {
        let session = SupervisorSession()
        self.session = session
        session.onReady = { [weak self] endpoints in
            guard let self, !self.terminating else { return }
            if let window = self.window {
                window.reload(baseURL: endpoints.baseURL)
                window.makeKeyAndOrderFront(nil)
            } else {
                self.showWindow(baseURL: endpoints.baseURL)
            }
        }
        session.onFailure = { [weak self] message in
            guard let self, !self.terminating else { return }
            self.presentFailure(message)
        }
        session.onUnexpectedExit = { [weak self] in
            guard let self, !self.terminating else { return }
            // 壳拉起的 Runtime 意外退出：原进程死亡后 lease 已释放，
            // 重新走收养/拉起流程，窗口复用 onReady 重载。
            self.startSession()
        }
        session.start()
    }

    private func showWindow(baseURL: URL) {
        let window = WebWindow(baseURL: baseURL)
        self.window = window
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func presentFailure(_ message: String) {
        let alert = NSAlert()
        alert.alertStyle = .critical
        alert.messageText = "QCode Runtime 不可用"
        alert.informativeText = message
        alert.addButton(withTitle: "重试")
        alert.addButton(withTitle: "退出")
        let handler: (NSApplication.ModalResponse) -> Void = { [weak self] response in
            if response == .alertSecondButtonReturn {
                NSApp.terminate(nil)
            } else {
                self?.startSession()
            }
        }
        if let window {
            alert.beginSheetModal(for: window) { handler($0) }
        } else {
            handler(alert.runModal())
        }
    }

    // MARK: - 生命周期

    // 关闭最后一个窗口不退出 App：Supervisor 常驻，点 Dock 图标可重新打开。
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }

    @discardableResult
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag, let window {
            window.makeKeyAndOrderFront(nil)
        }
        return true
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        SupervisorSession.log("applicationShouldTerminate entered")
        terminating = true
        guard let session else {
            SupervisorSession.log("no session, terminateNow")
            return .terminateNow
        }
        // 异步等待 Runtime drain（最多 ~35s），避免阻塞主线程被判定无响应。
        session.shutdown {
            DispatchQueue.main.async {
                SupervisorSession.log("shutdown complete, replying terminate")
                NSApp.reply(toApplicationShouldTerminate: true)
            }
        }
        SupervisorSession.log("applicationShouldTerminate returning terminateLater")
        return .terminateLater
    }

    func applicationWillTerminate(_ notification: Notification) {
        SupervisorSession.log("applicationWillTerminate")
    }

    // MARK: - 菜单

    // 刻意不提供 Reload 菜单项：误触 ⌘R 会丢前端内存态快照。
    // Edit 菜单必须保留标准项，否则 WKWebView 内 ⌘C/⌘V/⌘A/⌘Z 不生效。
    private func buildMenu() {
        let main = NSMenu()

        let appMenu = NSMenu()
        let appName = ProcessInfo.processInfo.processName
        appMenu.addItem(withTitle: "关于 \(appName)", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "隐藏 \(appName)", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: "隐藏其他", action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
        appMenu.items.last?.keyEquivalentModifierMask = [.command, .option]
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "退出 \(appName)", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        let appMenuItem = NSMenuItem()
        appMenuItem.submenu = appMenu
        main.addItem(appMenuItem)

        let editMenu = NSMenu(title: "编辑")
        editMenu.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = editMenu.addItem(withTitle: "重做", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        editMenu.addItem(.separator())
        editMenu.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        let editMenuItem = NSMenuItem()
        editMenuItem.submenu = editMenu
        main.addItem(editMenuItem)

        let windowMenu = NSMenu(title: "窗口")
        windowMenu.addItem(withTitle: "最小化", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        windowMenu.addItem(withTitle: "缩放", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        windowMenu.addItem(.separator())
        windowMenu.addItem(withTitle: "全部置于前", action: #selector(NSApplication.arrangeInFront(_:)), keyEquivalent: "")
        let windowMenuItem = NSMenuItem()
        windowMenuItem.submenu = windowMenu
        main.addItem(windowMenuItem)

        NSApp.mainMenu = main
    }
}
