import AppKit
import WebKit

// 主窗口：WKWebView 加载本地 Supervisor 的 http://127.0.0.1:<port>/。
// 不使用自定义 scheme——前端 WebSocket、服务端 Origin 栅栏和 localStorage
// 都要求稳定的 127.0.0.1 origin。
final class WebWindow: NSWindow {
    let webView: WKWebView
    private var titleObservation: NSKeyValueObservation?
    private let themeUnderlay = ThemeUnderlay()

    init(baseURL: URL) {
        let frame = NSRect(x: 0, y: 0, width: 1440, height: 900)
        let style: NSWindow.StyleMask = [.titled, .closable, .miniaturizable, .resizable]
        self.webView = WKWebView(frame: frame)
        super.init(contentRect: frame, styleMask: style, backing: .buffered, defer: false)

        minSize = NSSize(width: 980, height: 600)
        title = "QCode"
        contentView = webView

        let controller = webView.configuration.userContentController
        controller.addUserScript(NotificationBridge.userScript)
        controller.add(NotificationBridge.shared, name: "qcodeNotifications")
        themeUnderlay.webView = webView
        controller.add(themeUnderlay, name: "qcodeTheme")
        if ProcessInfo.processInfo.environment["QCODE_DESKTOP_DEV"] != nil {
            webView.configuration.preferences.setValue(true, forKey: "developerExtrasEnabled")
        }

        let delegates = WindowDelegates(baseURL: baseURL)
        objc_setAssociatedObject(self, &WindowDelegates.associationKey, delegates, .OBJC_ASSOCIATION_RETAIN)
        webView.navigationDelegate = delegates
        webView.uiDelegate = delegates

        NotificationBridge.shared.attach(webView: webView)
        themeUnderlay.applyInitial()

        // 前端后台活动监控会把 "(N) …" 写进 document.title；
        // 同步为窗口标题，并把计数映射成 dock 角标。
        titleObservation = webView.observe(\.title, options: [.new]) { [weak self] _, change in
            DispatchQueue.main.async {
                guard let self, let title = change.newValue ?? nil, !title.isEmpty else { return }
                self.title = title
                NSApplication.shared.dockTile.badgeLabel = Self.badgeCount(fromTitle: title)
            }
        }

        center()
        webView.load(URLRequest(url: baseURL))
    }

    func reload(baseURL: URL) {
        webView.load(URLRequest(url: baseURL))
    }

    private static func badgeCount(fromTitle title: String) -> String? {
        guard title.hasPrefix("("), let close = title.firstIndex(of: ")") else { return nil }
        let inner = title[title.index(after: title.startIndex)..<close]
        return inner.isEmpty ? nil : String(inner)
    }
}

// 主题底色桥：WKWebView 在合成层大幅重建（全屏遮罩 + 弹窗 + 嵌套弹窗
// 同时插入）的瞬间，可能露出一帧页面之下的原生底色——默认是白色，
// 暗色主题下表现为"明暗跳变"。underPageBackgroundColor 是 WebKit 为
// 此提供的公开 API；把底色对齐到前端画布色（tokens.css 的
// --ch-canvas），任何一帧的闪露都与页面同色而不可见。页面解析出
// 明暗主题后通过 qcodeTheme 消息回传纠正；加载前先按系统外观预设。
private final class ThemeUnderlay: NSObject, WKScriptMessageHandler {
    static let lightCanvas = NSColor(
        srgbRed: 0xF7 / 255.0, green: 0xF9 / 255.0, blue: 0xFC / 255.0, alpha: 1)
    static let darkCanvas = NSColor(
        srgbRed: 0x16 / 255.0, green: 0x19 / 255.0, blue: 0x1E / 255.0, alpha: 1)

    weak var webView: WKWebView?

    func applyInitial() {
        let dark = NSApp?.effectiveAppearance
            .bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
        apply(dark: dark)
    }

    private func apply(dark: Bool) {
        let color = dark ? Self.darkCanvas : Self.lightCanvas
        webView?.underPageBackgroundColor = color
        webView?.window?.backgroundColor = color
    }

    // MARK: WKScriptMessageHandler

    func userContentController(
        _ userContentController: WKUserContentController,
        didReceive message: WKScriptMessage
    ) {
        guard let theme = message.body as? String else { return }
        apply(dark: theme == "dark")
    }
}

// navigation/ui 代理拆成独立对象，通过 associated object 持有，
// 避免让 NSWindow 子类同时承担 WKWebView 代理职责。
private final class WindowDelegates: NSObject, WKNavigationDelegate, WKUIDelegate {
    static var associationKey: UInt8 = 0

    let baseURL: URL

    init(baseURL: URL) {
        self.baseURL = baseURL
    }

    // MARK: WKNavigationDelegate

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationAction: WKNavigationAction,
        decisionHandler: @escaping (WKNavigationActionPolicy) -> Void
    ) {
        guard let url = navigationAction.request.url else {
            decisionHandler(.cancel)
            return
        }
        // 同源（127.0.0.1 Supervisor）导航放行；其余一律交给系统浏览器。
        if isSameSite(url) {
            decisionHandler(.allow)
            return
        }
        if url.scheme == "http" || url.scheme == "https" {
            NSWorkspace.shared.open(url)
        }
        decisionHandler(.cancel)
    }

    private func isSameSite(_ url: URL) -> Bool {
        guard let host = url.host, let baseHost = baseURL.host else { return false }
        return url.scheme == baseURL.scheme && host == baseHost && url.port == baseURL.port
    }

    // MARK: WKUIDelegate

    // macOS WebKit 未实现此代理时会直接取消 <input type="file"> 请求。
    func webView(
        _ webView: WKWebView,
        runOpenPanelWith parameters: WKOpenPanelParameters,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping ([URL]?) -> Void
    ) {
        guard let window = webView.window else {
            completionHandler(nil)
            return
        }
        let panel = NSOpenPanel()
        panel.canChooseFiles = !parameters.allowsDirectories
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.beginSheetModal(for: window) { response in
            completionHandler(response == .OK ? panel.urls : nil)
        }
    }

    // target="_blank" 等新窗口请求：不在壳内开新窗口，外部链接走系统浏览器。
    func webView(
        _ webView: WKWebView,
        createWebViewWith configuration: WKWebViewConfiguration,
        for navigationAction: WKNavigationAction,
        windowFeatures: WKWindowFeatures
    ) -> WKWebView? {
        if let url = navigationAction.request.url, url.scheme == "http" || url.scheme == "https" {
            NSWorkspace.shared.open(url)
        }
        return nil
    }

    // 前端的删除会话、归档、重命名分别依赖 window.confirm / window.prompt /
    // window.alert，必须由 UI 代理呈现原生面板。
    func webView(
        _ webView: WKWebView,
        runJavaScriptAlertPanelWithMessage message: String,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping () -> Void
    ) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "好")
        runAlert(alert, for: webView) { _ in completionHandler() }
    }

    func webView(
        _ webView: WKWebView,
        runJavaScriptConfirmPanelWithMessage message: String,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping (Bool) -> Void
    ) {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = message
        alert.addButton(withTitle: "好")
        alert.addButton(withTitle: "取消")
        runAlert(alert, for: webView) { $0 == .alertFirstButtonReturn ? completionHandler(true) : completionHandler(false) }
    }

    func webView(
        _ webView: WKWebView,
        runJavaScriptTextInputPanelWithPrompt prompt: String,
        defaultText: String?,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping (String?) -> Void
    ) {
        let input = NSTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 24))
        input.stringValue = defaultText ?? ""
        let alert = NSAlert()
        alert.messageText = prompt
        alert.accessoryView = input
        alert.window.initialFirstResponder = input
        alert.addButton(withTitle: "好")
        alert.addButton(withTitle: "取消")
        runAlert(alert, for: webView) { response in
            completionHandler(response == .alertFirstButtonReturn ? input.stringValue : nil)
        }
    }

    private func runAlert(
        _ alert: NSAlert,
        for webView: WKWebView,
        completion: @escaping (NSApplication.ModalResponse) -> Void
    ) {
        if let window = webView.window {
            alert.beginSheetModal(for: window) { response in
                completion(response)
            }
        } else {
            completion(alert.runModal())
        }
    }
}

import ObjectiveC
