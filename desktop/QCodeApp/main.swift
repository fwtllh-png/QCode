import AppKit

// QCode 桌面壳入口：AppKit 程序化启动，无 Xcode 工程。
// 仓库构建脚本 desktop/build-app.sh 直接用 swiftc 编译本目录源文件。
let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
