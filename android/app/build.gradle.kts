// ═══ 更新日志 ═══
// 2026-11-15：版本号拒绝回落，发行构建校验既有签名证书且禁止自动创建新 keystore。
// 2026-10-13：初版——安卓 App 模块的构建配置。
//
// ═══ 三处不显然但必须这么写的地方 ═══
//
//  1. **jniLibs 里的「辅助程序」是 .so 但其实是可执行文件**。安卓的 App 私有目录
//     是 noexec，唯一能执行的位置是 APK 里 lib/<abi>/ 下的 lib*.so——系统安装时把它
//     解到 nativeLibraryDir，那里可执行。所以网关本体与辅助程序（login/credit/
//     fission_bin）都改名成 libwb2*.so 塞进 jniLibs。见 internal/android/helpers.go。
//
//  2. **不用 extractNativeLibs=false**。默认（true）系统会把 .so 解到文件系统上的
//     nativeLibraryDir 并给可执行权限；设成 false 时它们留在 APK 内、只能 dlopen，
//     起不了进程。
//
//  3. **minSdk 24**：Go 的 android/arm64 目标要求 API 21+，但前台服务与
//     `startForegroundService` 的行为在 24 之前差异太大（8.0 起还有通知渠道），
//     24 是让保活逻辑只有一条路径的最低线。
import java.security.KeyStore
import java.security.MessageDigest

plugins {
    id("com.android.application")
}

fun releaseVersionCode(name: String): Int {
    val match = Regex("(?:v)?(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)").matchEntire(name)
        ?: throw GradleException("Android 版本必须为 vX.Y.Z，禁止回落到 versionCode=1。")
    val parts = match.groupValues.drop(1).map {
        it.toLongOrNull() ?: throw GradleException("Android 版本号超出合法范围。")
    }
    if (parts[0] > 2100 || parts[1] >= 1000 || parts[2] >= 1000) {
        throw GradleException("Android 版本分段超出合法范围。")
    }
    val code = parts[0] * 1_000_000 + parts[1] * 1_000 + parts[2]
    if (code !in 3_016_001L..2_100_000_000L) {
        throw GradleException("Android versionCode 低于既有 v3.16.1 安装版本或超过平台上限。")
    }
    return code.toInt()
}

val sourceVersionFile = rootProject.file("../gateway/VERSION")
if (!sourceVersionFile.isFile) {
    throw GradleException("缺少 gateway/VERSION，不能猜测 Android 安装版本。")
}
val sourceVersion = sourceVersionFile.readText(Charsets.UTF_8).trim()
val sourceVersionCode = releaseVersionCode(sourceVersion)
val requestedVersion = (project.findProperty("versionName") as String?) ?: sourceVersion.removePrefix("v")
val derivedVersionCode = releaseVersionCode(requestedVersion)
val requestedCode = (project.findProperty("versionCode") as String?)?.let {
    it.toIntOrNull() ?: throw GradleException("versionCode 必须为合法整数。")
} ?: derivedVersionCode
if (requestedCode < maxOf(sourceVersionCode, derivedVersionCode) || requestedCode > 2_100_000_000) {
    throw GradleException("versionCode 不得低于当前源码/声明版本或超过 Android 上限。")
}
val externalWorkRoot = System.getenv("LOOMGATE_WORK_ROOT")?.let { file(it) }
    ?: if (System.getProperty("os.name").startsWith("Windows")) file("D:/LoomGate-work")
    else rootProject.file("../../LoomGate-work")
layout.buildDirectory.set(file((project.findProperty("buildOutputDir") as String?)
    ?: externalWorkRoot.resolve("build/android-gradle").path))

android {
    namespace = "com.workbuddy2api.app"
    compileSdk = 33

    defaultConfig {
        applicationId = "com.workbuddy2api.app"
        minSdk = 24
        targetSdk = 33
        // 版本由 android/build.py 从 gateway/VERSION 解析后注入（-PversionCode /
        // -PversionName）。**versionCode 必须每个版本递增**：安卓只按它判断
        // 「是不是新版本」，写死成 1 的话每次升级在系统看来都是同一个包，
        // 面板里显示已更新、实际装上去还是旧的，且用户无从分辨。
        versionCode = requestedCode
        versionName = requestedVersion.removePrefix("v")
        ndk {
            // ABI 由构建脚本传入（-PabiFilter）。默认只打 arm64：手机上的网关要跑
            // 整套 Go 服务，32 位设备内存不够。--abi x86_64 用于模拟器验证——
            // 但**只打一个**：jniLibs 下可能同时留着上一次为别的架构编的 .so
            // （--skip-go 复用产物时尤其容易），不限定就会把两份都塞进同一个 APK。
            val abi = (project.findProperty("abiFilter") as String?) ?: "arm64-v8a"
            abiFilters += listOf(abi)
        }
    }

    buildTypes {
        release {
            // 不混淆：这个 App 的全部代码就是起进程 + WebView，混淆收益为零，
            // 而它会让崩溃栈变成 a.b.c 这种无法定位的东西。
            isMinifyEnabled = false
            // 既有发行包使用此签名身份，必须继续沿用。下方会验证既有公开证书，
            // 缺少 keystore 时立即失败，不能让 AGP 生成另一把 debug key。
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_11
        targetCompatibility = JavaVersion.VERSION_11
    }

    // 构建产物保存在源码库之外；与 build.py 的 --skip-go 验证使用同一目录。
    sourceSets.getByName("main") {
        jniLibs.setSrcDirs(listOf(file((project.findProperty("jniLibsDir") as String?)
            ?: externalWorkRoot.resolve("build/android-jni").path)))
    }

    packagingOptions {
        // useLegacyPackaging=true：让系统把 .so 解到文件系统上（而不是留在 APK 里
        // 只供 dlopen）。我们的「.so」其实是可执行文件，必须落盘才能起进程。
        jniLibs {
            useLegacyPackaging = true
        }
    }
}

// 仅读取证书验证身份，不读取私钥，不把口令传到命令行或日志。
// 在任何打包任务执行之前检查，早于 AGP 可能自动生成 debug keystore 的步骤。
gradle.taskGraph.whenReady {
    val packagingKnownIdentity = allTasks.any {
        it.project == project && Regex("(assemble|bundle|package|validateSigning)(Release|Debug)").matches(it.name)
    }
    if (packagingKnownIdentity) {
        val existing = android.signingConfigs.getByName("debug")
        val store = existing.storeFile
        if (store == null || !store.isFile) {
            throw GradleException("缺少既有 Android 签名 keystore；请恢复原文件，禁止生成替代签名。")
        }
        val password = existing.storePassword?.toCharArray()
            ?: throw GradleException("既有 Android 签名配置不完整。")
        try {
            val keyStore = KeyStore.getInstance(existing.storeType ?: KeyStore.getDefaultType())
            store.inputStream().use { keyStore.load(it, password) }
            val certificate = keyStore.getCertificate(existing.keyAlias)
                ?: throw GradleException("既有 Android 签名证书不存在。")
            val fingerprint = MessageDigest.getInstance("SHA-256").digest(certificate.encoded)
                .joinToString("") { "%02x".format(it.toInt() and 0xff) }
            val legacyFingerprint = "44c18e220f1f6e505c2e702ff2a832cedb0e9be89e549d7874c136c0fb6fafef"
            if (fingerprint != legacyFingerprint) {
                throw GradleException("Android 签名身份与既有发行包不一致，拒绝构建不能覆盖升级的 APK。")
            }
        } catch (error: GradleException) {
            throw error
        } catch (error: Exception) {
            throw GradleException("无法验证既有 Android 公开签名证书，拒绝构建。")
        } finally {
            password.fill('\u0000')
        }
    }
}
