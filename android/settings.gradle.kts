// ═══ 更新日志 ═══
// 2026-10-13：初版——安卓端工程骨架（AGP 7.4.2 + Gradle 7.5，纯 Android SDK，不引 androidx）。
//
// 为什么不用 androidx / Kotlin：这个 App 的全部界面就是一个 WebView，逻辑是「起网关
// 进程 + 保活 + 打开面板」。为此引 appcompat + Kotlin 标准库要多出十几 MB 依赖和一套
// 插件链，收益为零。纯 SDK 的 Activity + Service 完全够用，构建也快得多。
pluginManagement {
    repositories {
        google()
        mavenCentral()
    }
}

dependencyResolutionManagement {
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "workbuddy2api"
include(":app")
