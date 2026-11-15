// ═══ 更新日志 ═══
// 2026-10-13：初版——安卓端的数据目录与二进制路径解析。
//
// 全部路径都从 App 私有目录推导，不写死任何绝对路径：
// 安卓给每个 App 分配的私有目录形如 `/data/user/0/<pkg>/files`，包名与用户 ID
// 都可能变（工作资料、多用户），写死就会在那些设备上直接崩。
package com.workbuddy2api.app;

import android.content.Context;

import java.io.File;

final class AppPaths {
    private AppPaths() {
    }

    /**
     * 数据根目录。
     *
     * 用 `getFilesDir()`（私有区）而不是 `getExternalFilesDir()`：
     * 后者在外部存储上，同一台设备上的其它 App 拿到存储权限后就能读到——而这里面
     * 是能直接调上游的账号凭据与 API 密钥。
     */
    static File dataRoot(Context context) {
        File root = new File(context.getFilesDir(), "data");
        ensureDir(root);
        return root;
    }

    static File configFile(Context context) {
        File dir = new File(dataRoot(context), "config");
        ensureDir(dir);
        return new File(dir, "config.json");
    }

    static File authsDir(Context context) {
        File dir = new File(dataRoot(context), "auths");
        ensureDir(dir);
        return dir;
    }

    static File panelDataDir(Context context) {
        File dir = new File(dataRoot(context), "panel-data");
        ensureDir(dir);
        return dir;
    }

    static File logDir(Context context) {
        File dir = new File(dataRoot(context), "logs");
        ensureDir(dir);
        return dir;
    }

    /**
     * 辅助程序（login / credit / fission_bin）所在的目录。
     *
     * 这是 APK 的 native lib 目录，由系统在安装时从 `lib/<abi>/libwb2*.so` 解出来。
     * **必须是这里**：应用私有目录是 noexec，只有 nativeLibraryDir 下的文件能执行。
     */
    static String nativeLibraryDir(Context context) {
        return context.getApplicationInfo().nativeLibraryDir;
    }

    /**
     * 网关本体。
     *
     * **优先用热更新下来的那份**：APK 的 nativeLibraryDir 是只读的，改不了，
     * 所以更新后的二进制落在数据目录（<data>/updates/），由网关自己写一个
     * `current` 指针指过去。这里每次启动都先看那个指针。
     *
     * 不看的话热更新就是假的：新版本会被下载、校验、交接成功，进程内一路正常
     * ——直到用户把 App 划掉再打开，又从 nativeLibraryDir 拉起 APK 里的旧二进制，
     * 版本悄悄回退。用户看到的是「更新了但重启又变回旧版」。
     */
    static String gatewayBinary(Context context) {
        File updated = updatedGateway(context);
        if (updated != null) {
            return updated.getAbsolutePath();
        }
        String dir = nativeLibraryDir(context);
        if (dir == null) {
            return null;
        }
        File binary = new File(dir, "libwb2gateway.so");
        return binary.isFile() ? binary.getAbsolutePath() : null;
    }

    /** 热更新下来的网关二进制；没有则 null。指针格式与 internal/android 一致。 */
    static File updatedGateway(Context context) {
        File pointer = new File(new File(dataRoot(context), "updates"), "current");
        if (!pointer.isFile()) {
            return null;
        }
        try {
            java.io.BufferedReader reader = new java.io.BufferedReader(
                    new java.io.InputStreamReader(new java.io.FileInputStream(pointer),
                            java.nio.charset.StandardCharsets.UTF_8));
            try {
                String line = reader.readLine();
                if (line == null) {
                    return null;
                }
                File binary = new File(line.trim());
                return binary.isFile() ? binary : null;
            } finally {
                reader.close();
            }
        } catch (java.io.IOException error) {
            android.util.Log.w(GatewayService.TAG, "读取更新指针失败", error);
            return null;
        }
    }

    /**
     * 面板的初始登录凭据文件（首次运行由网关写出）。
     *
     * 为什么要由 App 自己读给用户看：这个文件在**应用私有目录**里
     * （/data/user/0/<pkg>/files/...），非 root 手机上文件管理器读不到、adb 也
     * 读不到（`run-as` 只对 debuggable 包有效）。README 里「去这个路径看密码」
     * 的说法对普通用户是行不通的——用户装完根本登不进去。App 自己有权限，就该
     * 直接显示出来。
     */
    static File initialPasswordFile(Context context) {
        return new File(panelDataDir(context), "initial-password.txt");
    }

    /** 读初始凭据；不存在或读不到返回 null。 */
    static String readInitialPassword(Context context) {
        File file = initialPasswordFile(context);
        if (!file.isFile()) {
            return null;
        }
        try {
            java.io.BufferedReader reader = new java.io.BufferedReader(
                    new java.io.InputStreamReader(new java.io.FileInputStream(file),
                            java.nio.charset.StandardCharsets.UTF_8));
            try {
                StringBuilder text = new StringBuilder();
                String line;
                while ((line = reader.readLine()) != null) {
                    text.append(line).append('\n');
                }
                return text.toString().trim();
            } finally {
                reader.close();
            }
        } catch (java.io.IOException error) {
            android.util.Log.w(GatewayService.TAG, "读取初始口令失败", error);
            return null;
        }
    }

    private static void ensureDir(File dir) {
        if (!dir.isDirectory() && !dir.mkdirs() && !dir.isDirectory()) {
            // 建不出来只能靠后续操作报错（网关自己会在写配置时报更具体的原因）。
            // 这里不抛：抛了会让 App 在启动瞬间就崩，而崩溃栈里看不出是哪个目录。
            android.util.Log.w(GatewayService.TAG, "无法创建目录: " + dir);
        }
    }
}
