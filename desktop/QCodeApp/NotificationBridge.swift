import Foundation
import UserNotifications
import WebKit

// WKWebView 不实现 Web Notifications API，而前端的后台活动通知
// （web/src/ui/browserNotifications.ts）只用到了它的一个小子集：
// requestPermission / static permission / new Notification(title,{body,tag}) /
// onclick / close()。
//
// 这里在 document-start 注入一个 Notification 兼容 shim，把调用经
// WKScriptMessageHandler 转发到原生 UNUserNotificationCenter：
//  - requestPermission → 原生授权请求；
//  - new Notification → 原生通知（identifier 用 tag，天然按会话去重）；
//  - 点击通知 → evaluateJavaScript 回调 shim 实例的 onclick 并前置窗口。
// 前端的 localStorage 开关 ch.notifications.enabled 继续生效，语义不变。
enum NotificationBridge {
    static let permissionStorageKey = "qcode.desktop.notification.permission"
    static let messageName = "qcodeNotifications"

    static let shared = Bridge()
    static var userScript: WKUserScript { Bridge.userScript }

    final class Bridge: NSObject, WKScriptMessageHandler, UNUserNotificationCenterDelegate {
        private weak var webView: WKWebView?
        private var authorized = false

        static let userScript: WKUserScript = {
            let source = """
            (() => {
              if (window.Notification && typeof window.Notification.requestPermission === "function") {
                return;
              }
              const handler = window.webkit.messageHandlers.\(messageName);
              const instances = new Map();
              let permission = "default";
              try {
                const stored = window.localStorage.getItem("\(permissionStorageKey)");
                if (stored === "granted" || stored === "denied") { permission = stored; }
              } catch (err) {}
              const persist = (value) => {
                permission = value;
                try { window.localStorage.setItem("\(permissionStorageKey)", value); } catch (err) {}
              };
              class Notification {
                constructor(title, options = {}) {
                  this.title = title;
                  this.body = options.body || "";
                  this.tag = options.tag || "";
                  this.onclick = null;
                  instances.set(this.tag, this);
                  handler.postMessage({ kind: "show", title, body: this.body, tag: this.tag });
                }
                close() {
                  handler.postMessage({ kind: "close", tag: this.tag });
                  instances.delete(this.tag);
                }
                static get permission() { return permission; }
                static requestPermission() {
                  return new Promise((resolve) => {
                    window.__qcodeNotificationPermissionResolved = (value) => {
                      persist(value);
                      resolve(value);
                    };
                    handler.postMessage({ kind: "request" });
                  });
                }
              }
              window.Notification = Notification;
              window.__qcodeDispatchNotificationClick = (tag) => {
                const notice = instances.get(tag);
                if (notice && typeof notice.onclick === "function") { notice.onclick(); }
              };
            })();
            """
            return WKUserScript(source: source, injectionTime: .atDocumentStart, forMainFrameOnly: true)
        }()

        func attach(webView: WKWebView) {
            self.webView = webView
            let center = UNUserNotificationCenter.current()
            center.delegate = self
            center.getNotificationSettings { settings in
                self.authorized = settings.authorizationStatus == .authorized
            }
        }

        // MARK: WKScriptMessageHandler

        func userContentController(
            _ userContentController: WKUserContentController,
            didReceive message: WKScriptMessage
        ) {
            guard let body = message.body as? [String: Any], let kind = body["kind"] as? String else { return }
            switch kind {
            case "request":
                requestAuthorization()
            case "show":
                let title = body["title"] as? String ?? "QCode"
                let text = body["body"] as? String ?? ""
                let tag = body["tag"] as? String ?? ""
                show(title: title, body: text, tag: tag)
            case "close":
                let tag = body["tag"] as? String ?? ""
                UNUserNotificationCenter.current()
                    .removeDeliveredNotifications(withIdentifiers: [identifier(forTag: tag)])
            default:
                break
            }
        }

        private func requestAuthorization() {
            let center = UNUserNotificationCenter.current()
            center.requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
                self.authorized = granted
                self.evaluate("window.__qcodeNotificationPermissionResolved && window.__qcodeNotificationPermissionResolved(\(granted ? "\"granted\"" : "\"denied\""));")
            }
        }

        private func show(title: String, body: String, tag: String) {
            guard authorized else { return }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = body
            content.sound = .default
            let request = UNNotificationRequest(
                identifier: identifier(forTag: tag),
                content: content,
                trigger: nil
            )
            UNUserNotificationCenter.current().add(request)
        }

        private func identifier(forTag tag: String) -> String {
            "qcode.notification.\(tag)"
        }

        private func evaluate(_ script: String) {
            DispatchQueue.main.async { [weak self] in
                self?.webView?.evaluateJavaScript(script, completionHandler: nil)
            }
        }

        // MARK: UNUserNotificationCenterDelegate

        // App 在前台时也展示通知（与浏览器后台通知的预期一致），点击即聚焦。
        func userNotificationCenter(
            _ center: UNUserNotificationCenter,
            willPresent notification: UNNotification,
            withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
        ) {
            completionHandler([.banner, .sound])
        }

        func userNotificationCenter(
            _ center: UNUserNotificationCenter,
            didReceive response: UNNotificationResponse,
            withCompletionHandler completionHandler: @escaping () -> Void
        ) {
            let identifier = response.notification.request.identifier
            guard identifier.hasPrefix("qcode.notification.") else {
                completionHandler()
                return
            }
            let tag = String(identifier.dropFirst("qcode.notification.".count))
            DispatchQueue.main.async { [weak self] in
                guard let self else {
                    completionHandler()
                    return
                }
                NSApplication.shared.activate(ignoringOtherApps: true)
                if let window = self.webView?.window {
                    window.makeKeyAndOrderFront(nil)
                }
                let encoded = Self.jsonEncode(tag)
                self.webView?.evaluateJavaScript(
                    "window.__qcodeDispatchNotificationClick && window.__qcodeDispatchNotificationClick(\(encoded));",
                    completionHandler: { _, _ in completionHandler() }
                )
            }
        }

        private static func jsonEncode(_ value: String) -> String {
            guard let data = try? JSONSerialization.data(withJSONObject: [value]) else { return "\"\"" }
            let text = String(data: data, encoding: .utf8) ?? "[]"
            return String(text.dropFirst().dropLast())
        }
    }
}
