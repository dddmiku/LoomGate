// ═══ 更新日志 ═══
// 2026-10-13：初版——安卓端把网关当子进程拉起并保活。
//
// ═══ 为什么是子进程，不是「链接进 App」═══
//
// Go 可以直接编成 Android 库（gomobile），但那要求把网关重构成一个 library 入口，
// 还会让「桌面版跑的是哪个二进制」与「手机版跑的是哪个二进制」分叉——两套构建
// 产物、两套行为，出问题时要分别查。这里选择**同一个 cmd/server 二进制**改名成
// libwb2gateway.so 放进 jniLibs，运行时用 ProcessBuilder 起它。于是：
//
//   - 网关的行为在容器、桌面、手机上是同一份代码；
//   - 崩溃隔离：网关崩了 App 还在，能看到日志、能重启它；
//   - 不需要 gomobile、不需要 NDK。
//
// ═══ 保活的真实约束 ═══
//
// 安卓会杀后台进程。本项目要的是「排程任务按时跑」，所以网关必须常驻，做法是
// **前台服务 + 常驻通知**。除此之外还有两层系统限制要处理，缺一个就会出现
// 「过一会儿自己停了」：
//
//   1. **电池优化白名单**：不加白名单，Doze 模式下连前台服务也会被冻结。
//      这需要用户手动确认一次（系统弹窗），App 只能引导。
//   2. **后台启动限制**：Android 12+ 不允许后台启动前台服务，所以服务必须
//      在 Activity 可见时启动（用户点「启动」按钮那一刻）。
//
// 另外：进程被系统杀掉后，`START_STICKY` 会让服务被重建；但**重建时 Activity
// 可能不存在**，所以服务自己负责重新拉起网关子进程，不依赖界面。
package com.workbuddy2api.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.net.URL;
import java.nio.charset.StandardCharsets;

public class GatewayService extends Service {
    public static final String TAG = "wb2api";
    public static final String CHANNEL_ID = "wb2api-gateway";
    public static final int NOTIFICATION_ID = 0x5712;

    /** 网关监听地址。与安卓默认配置里的 listen 一致（只绑回环）。 */
    public static final String LISTEN_ADDR = "127.0.0.1:7863";
    public static final int LISTEN_PORT = 7863;

    /** 网关健康检查超时：Go 网关冷启动通常 1 秒内就绪，给 20 秒足够宽裕。 */
    private static final int READY_TIMEOUT_MS = 20000;

    private Process process;
    private Thread waiter;
    private volatile boolean stopping;
    private volatile long lastPendingStamp;

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        startForegroundCompat();
        watchForPendingInstall();
        // onStartCommand 跑在**主线程**上，而 startGateway 里要开 socket（探测端口、
        // 等 /healthz）。安卓禁止主线程做网络操作，直接抛 NetworkOnMainThreadException
        // ——2026-10-13 真机实测：点「启动」App 立刻崩，vivo 弹「运行异常」。
        // 整个启动序列必须挪到工作线程。
        Thread starter = new Thread(() -> {
            if (process == null || !process.isAlive()) {
                startGateway();
            }
        }, "wb2api-start");
        starter.setDaemon(true);
        starter.start();
        // START_STICKY：被系统杀掉后重建。重建时不带 intent（null），
        // 所以启动逻辑不能依赖 intent 的内容——上面就是这么写的。
        return START_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    @Override
    public void onDestroy() {
        stopping = true;
        stopGateway();
        super.onDestroy();
    }

    /** 当前网关是否在跑（界面用它显示状态）。 */
    public boolean isRunning() {
        return process != null && process.isAlive();
    }

    // ── 网关进程 ──────────────────────────────────────────────────────

    /**
     * 启动网关子进程。
     *
     * 环境变量与桌面版同源（cmd/desktop/main.go 也设这些），只多一个
     * WB2API_HELPER_DIR——安卓上辅助程序不在网关旁边，而在 nativeLibraryDir。
     */
    private void startGateway() {
        File root = AppPaths.dataRoot(this);
        File config = AppPaths.configFile(this);
        File auths = AppPaths.authsDir(this);
        File panelData = AppPaths.panelDataDir(this);
        File logs = AppPaths.logDir(this);

        String gateway = AppPaths.gatewayBinary(this);
        if (gateway == null) {
            notifyError("网关程序缺失：打包不完整");
            return;
        }

        ProcessBuilder builder = new ProcessBuilder(
                gateway,
                "-config", config.getAbsolutePath());
        builder.directory(root);
        builder.redirectErrorStream(true);
        builder.environment().put("WB2API_PANEL_ENABLED", "1");
        builder.environment().put("WB2API_PANEL_GO", "1");
        builder.environment().put("WB2API_RUNTIME", "native");
        // 安卓形态：网关据此自己准备数据目录与首次运行的配置（见
        // cmd/server/panel.go 的 BootstrapAndroidLayout）。默认值里的安卓取舍
        // 放在 Go 侧是单一事实来源，不让 Java 再复制一份 JSON。
        builder.environment().put("WB2API_ANDROID", "1");
        builder.environment().put("WB2API_GATEWAY_DIR", root.getAbsolutePath());
        builder.environment().put("WB2API_AUTHS_DIR", auths.getAbsolutePath());
        builder.environment().put("WB2API_ADMIN_DIR", panelData.getAbsolutePath());
        // 更新目录**显式传给网关**，而不是让两边各自推导。
        //
        // App 侧盯的是 <数据根>/updates（见 AppPaths / watchForPendingInstall），
        // 而网关侧 updateDir() 的优先级是 WB2API_UPDATE_DIR > 配置 update.dir >
        // 从 state_file 推导。不显式指定时，后两者恰好也落到同一处，但用户在配置
        // 里改了 update.dir、或环境里带了 WB2API_UPDATE_DIR，两边就分叉——症状是
        // 「下载与验签都成功、安装器始终不被拉起」，排查极难（v2.15.4 修过一次，
        // 那次是两侧各自推导导致的）。这里让「App 看哪里」与「网关写哪里」同源。
        builder.environment().put("WB2API_UPDATE_DIR",
                new File(root, "updates").getAbsolutePath());
        builder.environment().put("WB2API_CONFIG_PATH", config.getAbsolutePath());
        builder.environment().put("WB2API_RUNTIME_DIR", AppPaths.nativeLibraryDir(this));
        builder.environment().put("WB2API_HELPER_DIR", AppPaths.nativeLibraryDir(this));
        builder.environment().put("WB2API_LOG_PATH", new File(logs, "gateway.log").getAbsolutePath());
        builder.environment().put("WB2A_LISTEN", LISTEN_ADDR);
        // 手机上没有 nginx，直连对端恒为回环；可信代理只留回环，转发头一律不采信。
        builder.environment().put("WB2API_TRUSTED_PROXIES", "127.0.0.1/32,::1/128");
        builder.environment().put("HOME", root.getAbsolutePath());
        builder.environment().put("TMPDIR", root.getAbsolutePath());

        try {
            process = builder.start();
        } catch (IOException error) {
            Log.e(TAG, "启动网关失败", error);
            notifyError("启动网关失败：" + error.getMessage());
            return;
        }
        pipeLogs(process);
        waitForReady(process);
    }

    /** 把子进程的输出转到 logcat，便于用户用 adb 看，也便于 App 内展示。 */
    private void pipeLogs(Process target) {
        Thread thread = new Thread(() -> {
            try (BufferedReader reader = new BufferedReader(
                    new InputStreamReader(target.getInputStream(), StandardCharsets.UTF_8))) {
                String line;
                while ((line = reader.readLine()) != null) {
                    Log.i(TAG, line);
                }
            } catch (IOException ignored) {
                // 进程退出时流会断，这是正常路径。
            }
        }, "wb2api-logs");
        thread.setDaemon(true);
        thread.start();
    }

    /**
     * 等网关就绪；未就绪则把子进程杀掉并报错。
     *
     * 为什么必须等：网关加载账号、起管理 socket 需要时间，界面立刻打开 WebView 会
     * 撞上「连接被拒绝」，用户以为坏了。等到了再开界面，观感是「点一下，面板就出来了」。
     */
    private void waitForReady(Process target) {
        long deadline = System.currentTimeMillis() + READY_TIMEOUT_MS;
        while (System.currentTimeMillis() < deadline) {
            if (!target.isAlive()) {
                notifyError("网关启动失败（进程已退出），请在日志里查看原因");
                return;
            }
            if (healthOK()) {
                Log.i(TAG, "网关已就绪");
                return;
            }
            try {
                Thread.sleep(200);
            } catch (InterruptedException error) {
                Thread.currentThread().interrupt();
                return;
            }
        }
        Log.w(TAG, "网关未在超时内就绪");
        notifyError("网关启动超时，请查看日志");
    }

    /**
     * 探一次 /healthz，判断网关是否已经开始服务。
     *
     * **任何 HTTP 响应都算就绪，不要求 200**：`/healthz` 在**一个账号都没有**时
     * 返回 503（池里没有可服务的号），而那正是「刚装好、还没加号」的首次启动。
     * 只认 200 会让首启永远报「网关启动超时，请查看日志」，而面板其实已经能打开
     * ——2026-10-13 真机实测：日志报超时，界面里登录页却正常渲染，自相矛盾。
     *
     * 判据因此改成「连得上、且它回了 HTTP」：连不上会抛 IOException。
     */
    private boolean healthOK() {
        HttpURLConnection connection = null;
        try {
            URL url = new URL("http://" + LISTEN_ADDR + "/healthz");
            connection = (HttpURLConnection) url.openConnection();
            connection.setConnectTimeout(500);
            connection.setReadTimeout(1500);
            connection.setRequestMethod("GET");
            int code = connection.getResponseCode();
            // 网关的身份标识：有 X-Service 头就确认是本网关（而不是同端口上别的服务）。
            String service = connection.getHeaderField("X-Service");
            if (service == null || service.isEmpty()) {
                service = connection.getHeaderField("x-service");
            }
            return code > 0 && (service == null || service.isEmpty() || "workbuddy2api".equals(service));
        } catch (IOException error) {
            return false;
        } finally {
            if (connection != null) {
                connection.disconnect();
            }
        }
    }

    /** 停掉网关：先给 SIGTERM 的机会，超时再强杀。 */
    private void stopGateway() {
        if (process == null) {
            return;
        }
        process.destroy(); // Java 在 Linux/Android 上发 SIGTERM
        try {
            if (!process.waitFor(5, java.util.concurrent.TimeUnit.SECONDS)) {
                process.destroyForcibly();
            }
        } catch (InterruptedException error) {
            process.destroyForcibly();
            Thread.currentThread().interrupt();
        }
        process = null;
    }

    /**
     * 检查网关是否下载好了更新包，有就拉起系统安装器。
     *
     * 网关是 App 的子进程，没有 Android Context、拉不起 startActivity，所以它只能
     * 把校验过的 APK 放到 <data>/updates/pending-install.apk 并写日志；起安装器
     * 这件事由 App 做（见 launchInstaller 的注释）。
     *
     * 用轮询而不是文件监听：更新是低频事件，而 FileObserver 在各家 ROM 上的
     * 行为差异很大（有的需要额外权限）。30 秒一次的开销可以忽略。
     */
    private void watchForPendingInstall() {
        Thread watcher = new Thread(() -> {
            File pending = new File(new File(AppPaths.dataRoot(this), "updates"), "pending-install.apk");
            while (!stopping) {
                try {
                    Thread.sleep(5000);
                } catch (InterruptedException error) {
                    Thread.currentThread().interrupt();
                    return;
                }
                if (!pending.isFile()) {
                    continue;
                }
                // 记录已处理过的文件大小与时间，避免同一份被反复拉起来。
                long stamp = pending.length() * 31 + pending.lastModified();
                if (stamp == lastPendingStamp) {
                    continue;
                }
                lastPendingStamp = stamp;
                // **不在这里起安装器**：Android 10+ 禁止后台启动 Activity，
                // Service 起的 startActivity 会被静默丢弃（vivo 上连异常都不抛，
                // 表现为「什么都没发生」）。改成**广播给界面**，由 MainActivity
                // 在前台弹一个「是否安装」对话框——点「安装」是用户手势，任何 ROM
                // 都不会拦（见 MainActivity.promptInstallIfPending 的注释）。
                notifyStatus("新版本已下载，等待确认安装");
                Intent prompt = new Intent(MainActivity.ACTION_PENDING_INSTALL);
                prompt.setPackage(getPackageName());
                sendBroadcast(prompt);
                Log.i(TAG, "pending install ready: " + pending.getAbsolutePath());
            }
        }, "wb2api-install-watch");
        watcher.setDaemon(true);
        watcher.start();
    }

    /** 把一条状态发给界面（服务可能不在前台，用广播即可）。 */
    private void notifyStatus(String message) {
        Log.i(TAG, message);
        Intent broadcast = new Intent(MainActivity.ACTION_STATUS);
        broadcast.setPackage(getPackageName());
        broadcast.putExtra(MainActivity.EXTRA_STATUS, message);
        sendBroadcast(broadcast);
    }

    /** 等待网关退出（重启流程用）。 */
    public void awaitExit() {
        if (process == null) {
            return;
        }
        try {
            process.waitFor();
        } catch (InterruptedException error) {
            Thread.currentThread().interrupt();
        }
    }

    /**
     * 重启网关（面板改完配置后调用）。
     *
     * 同样要离开主线程：它内部会等 /healthz（开 socket）。
     */
    public void restart() {
        Thread restarter = new Thread(() -> {
            stopGateway();
            startGateway();
        }, "wb2api-restart");
        restarter.setDaemon(true);
        restarter.start();
    }

    // ── 前台服务与通知 ────────────────────────────────────────────────

    private void createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return;
        }
        NotificationManager manager = getSystemService(NotificationManager.class);
        if (manager == null) {
            return;
        }
        NotificationChannel channel = new NotificationChannel(
                CHANNEL_ID, "网关运行状态", NotificationManager.IMPORTANCE_LOW);
        // LOW：常驻通知不该响铃或弹窗，它只是「服务在跑」的告知。
        channel.setShowBadge(false);
        manager.createNotificationChannel(channel);
    }

    private void startForegroundCompat() {
        Notification.Builder builder = (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
                ? new Notification.Builder(this, CHANNEL_ID)
                : new Notification.Builder(this);
        Intent open = new Intent(this, MainActivity.class);
        int flags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            flags |= PendingIntent.FLAG_IMMUTABLE;
        }
        builder.setContentTitle("WorkBuddy 网关运行中")
                .setContentText("点按打开管理面板")
                .setSmallIcon(android.R.drawable.stat_sys_upload_done)
                .setOngoing(true)
                .setContentIntent(PendingIntent.getActivity(this, 0, open, flags));
        Notification notification = builder.build();
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(NOTIFICATION_ID, notification,
                    ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC);
        } else {
            startForeground(NOTIFICATION_ID, notification);
        }
    }

    /**
     * 待安装的更新包路径；没有则 null。
     *
     * 由 MainActivity 在前台时检查（见 launchInstaller 的注释：Android 10+ 禁止
     * 后台启动 Activity，Service 里起 startActivity 会被静默丢弃）。
     */
    static java.io.File pendingInstall(Context context) {
        java.io.File pending = new java.io.File(new java.io.File(AppPaths.dataRoot(context), "updates"), "pending-install.apk");
        return pending.isFile() ? pending : null;
    }

    /**
     * 把热更新下载下来的 APK 交给系统安装器。
     *
     * ═══ 为什么安卓必须走这条路，而不是像 Linux 那样交接进程 ═══
     *
     * 应用私有目录（/data/data/<pkg>/files）里的文件**不能执行**——不是权限位的
     * 问题，是 SELinux 的 untrusted_app 域禁止在 app_data_file 上 execute。
     * 2026-10-13 真机实测：`fork/exec data/updates/v2.15.1-...: permission denied`，
     * 且 `setenforce 0` 也要 root。唯一可执行的位置是 APK 的 nativeLibraryDir，
     * 而它是只读的、改不了。
     *
     * 所以安卓的自更新走系统机制：把新 APK 交给系统安装器，用户确认后 Android
     * 原地替换（**应用数据保留**），新的 nativeLibraryDir 里就是新二进制。
     * 这条路不需要 root，也是安卓上唯一被支持的自更新方式。
     *
     * **必须在前台调用**：Android 10+ 禁止后台启动 Activity，Service 里起
     * startActivity 会被静默丢弃（见 watchForPendingInstall 的注释）。
     */
    static String launchInstaller(Context context, java.io.File apk) {
        if (apk == null || !apk.isFile()) {
            return "更新包不存在";
        }
        // 必须用 content:// URI：Android 7 起把 file:// URI 传给另一个进程会抛
        // FileUriExposedException，而 packageinstaller 正是另一个进程。
        //
        // ApkProvider 是我们自己的最小实现（androidx 的 FileProvider 需要引依赖，
        // 而这个 App 刻意不引 androidx）。它必须实现 query() 返回
        // OpenableColumns——安装器靠它拿到应用名与大小来渲染确认页，返回 null 时
        // 确认页里应用名显示为「未知」。
        //
        // 另外，**清单里必须有 REQUEST_INSTALL_PACKAGES**，否则安装器会静默退出：
        // 界面一闪而过、不弹确认框、logcat 里也没有任何记录，用户看到的是
        // 「点了没反应」——2026-10-13 真机实测（vivo V2020A / Android 10）就是这个
        // 症状，加上该权限后确认框正常出现。
        java.io.File external = context.getExternalCacheDir();
        java.io.File base = external != null ? external : context.getCacheDir();
        java.io.File dir = new java.io.File(base, "install");
        if (!dir.isDirectory() && !dir.mkdirs() && !dir.isDirectory()) {
            return "无法创建安装包暂存目录：" + dir;
        }
        java.io.File staged = new java.io.File(dir, apk.getName());
        try {
            copyFile(apk, staged);
        } catch (java.io.IOException error) {
            Log.e(TAG, "复制安装包失败", error);
            return "复制安装包失败：" + error.getMessage();
        }
        android.net.Uri uri = new android.net.Uri.Builder()
                .scheme("content")
                .authority(ApkProvider.AUTHORITY)
                .appendPath(staged.getName())
                .build();
        Intent intent = new Intent(Intent.ACTION_VIEW);
        intent.setDataAndType(uri, "application/vnd.android.package-archive");
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        intent.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
        try {
            Log.i(TAG, "拉起系统安装器：" + uri);
            context.startActivity(intent);
            return "已交给系统安装器：" + apk.getName();
        } catch (Exception error) {
            // 到这一步还能失败，只可能是系统没有可处理该 Intent 的安装器
            // （被 ROM 精简掉），或者用户在系统设置里关掉了「安装未知来源应用」。
            Log.e(TAG, "拉起安装器失败", error);
            return "无法打开系统安装器，请在系统设置里允许本应用「安装未知来源应用」后重试";
        }
    }

    /** 复制文件（安装包搬到安装器读得到的位置）。 */
    static void copyFile(java.io.File source, java.io.File target) throws java.io.IOException {
        try (java.io.InputStream in = new java.io.FileInputStream(source);
             java.io.OutputStream out = new java.io.FileOutputStream(target)) {
            byte[] buffer = new byte[64 * 1024];
            int read;
            while ((read = in.read(buffer)) > 0) {
                out.write(buffer, 0, read);
            }
        }
    }

    private void notifyError(String message) {
        Log.e(TAG, message);
        Intent broadcast = new Intent(MainActivity.ACTION_STATUS);
        broadcast.setPackage(getPackageName());
        broadcast.putExtra(MainActivity.EXTRA_STATUS, message);
        sendBroadcast(broadcast);
    }

    // ── 静态入口 ──────────────────────────────────────────────────────

    /** 启动服务（必须在 Activity 可见时调用：Android 12+ 禁止后台起前台服务）。 */
    public static void start(Context context) {
        Intent intent = new Intent(context, GatewayService.class);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            context.startForegroundService(intent);
        } else {
            context.startService(intent);
        }
    }

    public static void stop(Context context) {
        context.stopService(new Intent(context, GatewayService.class));
    }

    /**
     * 端口是否已被占用（判断网关是否在跑）。
     *
     * **必须在后台线程调用**：它开 TCP 连接，而安卓在 API 11 之后禁止在主线程做
     * 网络操作（StrictMode 直接抛 NetworkOnMainThreadException）。界面在 onResume
     * 里刷新状态，一不小心就会踩上——2026-10-13 在模拟器上实测：App 一启动就
     * FATAL EXCEPTION 崩掉，`at com.workbuddy2api.app.GatewayService.portInUse`。
     */
    public static boolean portInUse() {
        if (Looper.myLooper() == Looper.getMainLooper()) {
            // 在 UI 线程上不冒险，直接说「未知」。调用方会再查一次——
            // 与其崩掉，不如让界面晚一拍显示状态。
            return false;
        }
        try (Socket socket = new Socket()) {
            socket.connect(new InetSocketAddress("127.0.0.1", LISTEN_PORT), 300);
            return true;
        } catch (IOException error) {
            return false;
        }
    }

    /** 端口是否已被占用；结果回调在主线程。 */
    public static void portInUseAsync(final java.util.function.Consumer<Boolean> callback) {
        Thread worker = new Thread(() -> {
            final boolean running = portInUse();
            new android.os.Handler(Looper.getMainLooper()).post(() -> callback.accept(running));
        }, "wb2api-port-check");
        worker.setDaemon(true);
        worker.start();
    }
}
