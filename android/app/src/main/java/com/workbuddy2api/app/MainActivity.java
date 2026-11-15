// ═══ 更新日志 ═══
// 2026-10-13：去掉顶部按钮条，面板全屏；改为自动登录（用户不再需要手抄口令）；
//             更新包下载完成后自动弹「是否安装」对话框。
//
// ═══ 为什么不再有顶部按钮条 ═══
//
// 初版在 WebView 上方放了一条「状态 / 面板 / 凭据 / 安装更新」按钮栏。实测下来
// 这条栏本身就是问题来源：
//
//   - 它占了本来就紧张的竖屏高度，手机上看着像没做完的半成品；
//   - 「面板」按钮在面板已经全屏显示时是无意义的重复入口；
//   - 「凭据」按钮存在的原因只是**用户需要手抄口令去登录页**——而这件事本可以
//     由 App 自己做掉（见 autoLogin）；
//   - 「安装更新」按钮要求用户下载完还得退回首页点一下，流程割裂。
//
// 所以四个按钮的去向分别是：状态并入面板（面板「系统」页本来就有容器状态）、
// 面板去掉（全屏就是面板）、凭据去掉（自动登录，口令用户根本不用看见）、
// 安装改成下载完直接弹窗。界面于是只剩一个全屏 WebView。
//
// ═══ 两条必须引导用户的系统设置 ═══
//
//   1. **电池优化白名单**（`REQUEST_IGNORE_BATTERY_OPTIMIZATIONS`）：不加白名单，
//      Doze 模式下前台服务也会被冻结，排程任务就不再按时跑。
//   2. **通知权限**（Android 13+）：前台服务的通知不显示时，服务会被系统降级甚至
//      杀掉，症状是「App 过一会儿自己停了」。
//
// 两者都必须由用户在系统弹窗里确认，App 只能引导（这也是它们没有被做成静默申请的原因）。
package com.workbuddy2api.app;

import android.Manifest;
import android.annotation.SuppressLint;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.Settings;
import android.util.Log;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.CookieManager;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;

public class MainActivity extends Activity {
    public static final String ACTION_STATUS = "com.workbuddy2api.app.STATUS";
    public static final String EXTRA_STATUS = "status";
    /** 网关下载好更新包时由服务发出；界面在前台就立刻弹安装对话框。 */
    public static final String ACTION_PENDING_INSTALL = "com.workbuddy2api.app.PENDING_INSTALL";

    private static final int REQUEST_NOTIFICATIONS = 1001;
    private static final String PANEL_URL = "http://" + GatewayService.LISTEN_ADDR + "/admin/";

    /** 本次 Activity 生命周期里是否已经尝试过自动启动（防止重建时重复探测）。 */
    private boolean autoStarted;
    /** 自动登录是否已经成功过；失败才回落到「显示凭据」的兜底路径。 */
    private boolean autoLoginTried;
    /**
     * 面板是否已经加载过。
     *
     * ═══ 为什么需要这个标记 ═══
     *
     * 面板是单页应用，「添加账号」是一个**多步抽屉**（选区域 → 生成链接 → 授权 →
     * 粘贴回调）。用户去浏览器完成授权时 App 会退到后台，回来时 onResume 触发
     * refreshState，而它此前无条件调用 openPanel → loadUrl，于是**整个面板被重新
     * 加载**——抽屉关闭、回到账号列表，用户刚走到第 2 步就被踢回起点，永远加不完。
     * （2026-10-13 用户实测：「划到后台后，添加账号两个步骤那个页面会自动返回到
     * 账号页，不给添加的机会」。）
     *
     * 所以只在**确实需要**时加载：首次打开，或网关从「没跑」变成「在跑」。
     * 单纯的「回到前台」绝不能重新加载。
     */
    private boolean panelLoaded;
    /** 上一次观察到的网关运行状态，用来识别「从没跑到在跑」的跃迁。 */
    private boolean gatewayWasRunning;
    private long installedStamp;
    /** 已经弹过安装框的那份包，避免同一份反复打扰。 */
    private long promptedStamp;
    private WebView webView;
    /** 网关没起来时盖在 WebView 上的占位层（含启动按钮）。 */
    private View startPanel;
    private TextView statusView;
    private Button startButton;

    private final BroadcastReceiver statusReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            String message = intent.getStringExtra(EXTRA_STATUS);
            if (message != null && statusView != null) {
                statusView.setText(message);
            }
        }
    };

    private final BroadcastReceiver installReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            // 服务轮询到新包时广播过来。此时 App 在前台（接收器在 onResume 注册），
            // 弹对话框是安全的。
            promptInstallIfPending();
        }
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(buildUi());
        requestNotificationPermission();
        autoStartGateway();
    }

    /**
     * 打开 App 就自动把网关拉起来，不用用户每次点「启动」。
     *
     * ═══ 为什么放在 onCreate 而不是 onResume ═══
     *
     * onResume 每次回到前台都会跑（从面板、从系统设置返回…），在那里自动启动会
     * 反复探测端口；onCreate 只在 Activity 真正被创建时跑一次，正好对应「用户打开了
     * App」这个语义。
     *
     * ═══ 为什么必须先探测端口 ═══
     *
     * 服务是 START_STICKY 的，网关很可能**已经在跑**（上次没被杀、或系统刚重建过
     * 服务）。不探测就 start 会撞端口，网关自己会退出，用户看到的是「启动失败」。
     *
     * ═══ 为什么不在这里申请电池白名单 ═══
     *
     * 那个系统弹窗会打断「打开就能用」的体验，而且它只在**首次**真正需要引导时
     * 才有意义。改由用户在占位层的「启动」按钮里主动触发（见 onStartClicked）。
     */
    private void autoStartGateway() {
        if (autoStarted) {
            return;
        }
        autoStarted = true;
        // 端口探测开 TCP 连接，必须离开主线程（见 GatewayService.portInUse 的注释）。
        GatewayService.portInUseAsync(running -> {
            if (running) {
                onGatewayReady();
                return;
            }
            GatewayService.start(this);
            showStarting();
            // 等就绪再切到面板：立刻加载会撞上「连接被拒绝」。
            startButton.postDelayed(this::refreshState, 1500);
        });
    }

    @Override
    protected void onResume() {
        super.onResume();
        maybeOfferCredentials();
        IntentFilter filter = new IntentFilter(ACTION_STATUS);
        IntentFilter installs = new IntentFilter(ACTION_PENDING_INSTALL);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            registerReceiver(statusReceiver, filter, Context.RECEIVER_NOT_EXPORTED);
            registerReceiver(installReceiver, installs, Context.RECEIVER_NOT_EXPORTED);
        } else {
            registerReceiver(statusReceiver, filter);
            registerReceiver(installReceiver, installs);
        }
        refreshState();
        // 回到前台时也查一次：App 在后台期间下载完的包，不能等到下次广播。
        promptInstallIfPending();
    }

    @Override
    protected void onPause() {
        unregisterReceiver(statusReceiver);
        unregisterReceiver(installReceiver);
        super.onPause();
    }

    @Override
    public void onBackPressed() {
        // WebView 里有历史时先回退，否则用户一按返回就退出 App、
        // 而面板里的「返回」按钮会让人以为卡住了。
        if (webView != null && webView.canGoBack()) {
            webView.goBack();
            return;
        }
        super.onBackPressed();
    }

    // ── 界面 ──────────────────────────────────────────────────────────

    /**
     * 界面只有一个全屏 WebView，外加一层「网关没起来」时盖上去的占位层。
     *
     * 没有顶部按钮条（见文件头注释）。占位层平时是 GONE，只有网关确实没在跑时
     * 才显示——它承载了唯一的原生按钮「启动」，因为那是自动启动失败后用户
     * 唯一的自救入口，不能省。
     */
    private View buildUi() {
        FrameLayout root = new FrameLayout(this);

        webView = buildWebView();
        root.addView(webView, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));

        startPanel = buildStartPanel();
        startPanel.setVisibility(View.GONE);
        root.addView(startPanel, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        return root;
    }

    private View buildStartPanel() {
        LinearLayout column = new LinearLayout(this);
        column.setOrientation(LinearLayout.VERTICAL);
        column.setGravity(Gravity.CENTER);
        column.setPadding(dp(28), dp(28), dp(28), dp(28));

        statusView = new TextView(this);
        statusView.setText("正在检查网关状态…");
        statusView.setGravity(Gravity.CENTER);
        column.addView(statusView);

        startButton = new Button(this);
        startButton.setText("启动网关");
        startButton.setOnClickListener(view -> onStartClicked());
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT);
        params.topMargin = dp(18);
        column.addView(startButton, params);
        return column;
    }

    @SuppressLint("SetJavaScriptEnabled")
    private WebView buildWebView() {
        WebView view = new WebView(this);
        WebSettings settings = view.getSettings();
        // 面板前端需要 JS 与 localStorage（主题偏好）；localStorage 默认就是开的，
        // 这里显式写出来是为了让「面板需要什么」在这一处能看全。
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        // 面板是自包含的单文件（build.py 把 CSS/JS 都内联了），不需要文件访问。
        settings.setAllowFileAccess(false);
        settings.setAllowContentAccess(false);
        view.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView ignored, WebResourceRequest request) {
                return routeNavigation(request.getUrl());
            }

            @Override
            @SuppressWarnings("deprecation") // 老 WebView 只回调这个重载
            public boolean shouldOverrideUrlLoading(WebView ignored, String url) {
                return routeNavigation(Uri.parse(url));
            }

            @Override
            public void onReceivedError(WebView ignored, WebResourceRequest request,
                                        android.webkit.WebResourceError error) {
                // 主文档加载失败（网关还没起来 / 刚被重启）：清掉「已加载」标记，
                // 让下一次 onResume 的重试能真的重新加载，而不是停在一个错误页上
                // 再也不动。子资源失败不算。
                if (request != null && request.isForMainFrame()) {
                    Log.w(GatewayService.TAG, "面板主文档加载失败，允许下次重试");
                    panelLoaded = false;
                }
            }

            @Override
            @SuppressWarnings("deprecation")
            public void onReceivedError(WebView ignored, int errorCode, String description,
                                        String failingUrl) {
                Log.w(GatewayService.TAG, "面板加载失败：" + description);
                panelLoaded = false;
            }
        });
        // window.open 在多窗口关闭时（默认）会退化成同窗口跳转，走上面的
        // shouldOverrideUrlLoading；打开多窗口时走 onCreateWindow。两条都接住，
        // 免得某天 ROM 或 WebView 版本改了默认行为就漏掉外链。
        view.setWebChromeClient(new android.webkit.WebChromeClient() {
            @Override
            public boolean onCreateWindow(WebView ignored, boolean isDialog, boolean isUserGesture,
                                          android.os.Message resultMsg) {
                // 不创建 WebView 窗口：外链一律交给系统浏览器（见 routeNavigation）。
                return false;
            }
        });
        return view;
    }

    /**
     * 决定一次导航留在 WebView 还是交给系统浏览器。
     *
     * ═══ 为什么外链必须出 WebView ═══
     *
     * Trae 授权流程的最后一步是「授权后浏览器跳到 127.0.0.1:8765，把**地址栏里的
     * 整条链接**复制回来」。而 WebView 里**没有地址栏**——授权页一旦在 WebView 内
     * 打开，用户拿不到那条链接，整个添加流程就断在这一步（2026-10-13 用户实测：
     * 「没有跳转浏览器，直接内部打开了没法复制地址栏的链接」）。
     *
     * 所以判据是**按 host+port 分**：只有本机面板（127.0.0.1:7863）留在 WebView，
     * 其余一律 `ACTION_VIEW` 交给系统浏览器。这样授权页在一个有地址栏的真实浏览器
     * 里打开，回调失败页上的链接也能直接复制。
     *
     * 返回 true 表示「我已经处理了这次导航，WebView 不要再加载」。
     */
    private boolean routeNavigation(Uri uri) {
        if (uri == null) {
            return false;
        }
        if (isPanelUrl(uri)) {
            return false; // 面板自己的页面：留在 WebView
        }
        openInBrowser(uri);
        return true;
    }

    /** 是否为本机面板的地址（只有它该留在 WebView 里）。 */
    private static boolean isPanelUrl(Uri uri) {
        return "http".equalsIgnoreCase(uri.getScheme())
                && "127.0.0.1".equals(uri.getHost())
                && uri.getPort() == GatewayService.LISTEN_PORT;
    }

    /** 用系统浏览器打开外链。失败时给出可复制的地址，不留死路。 */
    private void openInBrowser(Uri uri) {
        try {
            Intent intent = new Intent(Intent.ACTION_VIEW, uri);
            intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
            startActivity(intent);
        } catch (Exception error) {
            Log.w(GatewayService.TAG, "打开外部浏览器失败", error);
            // 没有浏览器（或 ROM 拦截）时至少让链接可复制，别让用户彻底卡住。
            copyToClipboard(uri.toString());
            toast("无法打开浏览器，链接已复制，请手动粘贴到浏览器");
        }
    }

    // ── 行为 ──────────────────────────────────────────────────────────

    /** 网关已就绪：藏掉占位层，**必要时**才加载面板。 */
    private void onGatewayReady() {
        startPanel.setVisibility(View.GONE);
        gatewayWasRunning = true;
        if (panelLoaded) {
            // 面板已经加载过：什么都不做。这里就是「从浏览器切回来不该被重置」的
            // 那道闸——见 panelLoaded 的注释。
            return;
        }
        openPanel();
    }

    /** 网关还没起来：显示占位层（含「启动网关」按钮）。 */
    private void showStarting() {
        gatewayWasRunning = false;
        if (startPanel.getVisibility() != View.VISIBLE) {
            startPanel.setVisibility(View.VISIBLE);
        }
    }

    private void onStartClicked() {
        startButton.setEnabled(false);
        // 端口探测开 TCP 连接，必须离开主线程（见 GatewayService.portInUse 的注释）。
        GatewayService.portInUseAsync(running -> {
            startButton.setEnabled(true);
            if (running) {
                // 已经在跑：不重复起进程（那会撞端口，网关自己会退出，用户看到「启动失败」）。
                onGatewayReady();
                return;
            }
            requestBatteryExemption();
            GatewayService.start(this);
            statusView.setText("正在启动网关…");
            // 等就绪再切到面板：立刻加载会撞上「连接被拒绝」。
            startButton.postDelayed(this::refreshState, 1500);
        });
    }

    /**
     * 打开面板。**先自动登录再加载**，否则用户会被面板的登录页拦住。
     *
     * 登录是 App 用自己的身份替用户做的：口令就在应用私有目录里，用户本来就
     * 读不到（见 AppPaths.initialPasswordFile），让他手抄一遍只是白白多一道坎。
     *
     * 只应由「首次打开」或「网关从没跑到在跑」调用——反复调用会把面板里正在进行的
     * 操作（比如添加账号的多步抽屉）重置掉，见 panelLoaded 的注释。
     */
    private void openPanel() {
        GatewayService.portInUseAsync(running -> {
            if (!running) {
                showStarting();
                statusView.setText("网关未运行");
                return;
            }
            startPanel.setVisibility(View.GONE);
            if (autoLoginTried) {
                loadPanel();
                return;
            }
            autoLoginTried = true;
            autoLogin(url -> {
                if (!url) {
                    // 自动登录失败：面板会显示登录页，用户需要口令才能进去。
                    // 那口令在应用私有目录里、他自己读不到，所以此刻**必须**把凭据
                    // 弹给他——否则他就卡在登录页上，而且没有任何入口能拿到口令
                    // （顶部按钮条已经去掉了，见文件头注释）。
                    Log.w(GatewayService.TAG, "自动登录失败，显示凭据供手动登录");
                    showCredentials();
                }
                loadPanel();
            });
        });
    }

    /** 真正加载面板，并记住「已加载」（后续 onResume 不再重新加载）。 */
    private void loadPanel() {
        panelLoaded = true;
        webView.loadUrl(PANEL_URL);
    }

    /**
     * 用初始凭据换一个管理会话 cookie，并把它塞进 WebView 的 CookieManager。
     *
     * ═══ 为什么要 App 自己做，而不是让 WebView 自动填表 ═══
     *
     * 面板的登录是「POST /api/auth/login → 服务端 Set-Cookie」，cookie 是 HttpOnly 的。
     * 让 WebView 去填表单需要注入 JS 操作 DOM，既脆（面板改版就失效）又把口令暴露在
     * 页面上下文里。这里直接用 HttpURLConnection 打一次 API，拿 Set-Cookie 原样灌进
     * CookieManager——**不经过页面**，口令不出现在任何 DOM 里。
     *
     * HttpOnly 不影响这一步：它只禁止**页面 JS** 读取 cookie，CookieManager 作为宿主
     * 仍然可以设置它。
     *
     * ═══ 为什么必须在加载面板之前完成 ═══
     *
     * 面板对未登录请求是 302 到 login 页。先把 cookie 放好再 loadUrl，用户看到的是
     * 直接进面板；顺序反了就会先闪一下登录页再跳回来。
     */
    private void autoLogin(final java.util.function.Consumer<Boolean> done) {
        new Thread(() -> {
            boolean ok = false;
            try {
                ok = performAutoLogin();
            } catch (Exception error) {
                Log.w(GatewayService.TAG, "自动登录异常", error);
            }
            final boolean success = ok;
            new Handler(Looper.getMainLooper()).post(() -> done.accept(success));
        }, "wb2api-autologin").start();
    }

    private boolean performAutoLogin() throws Exception {
        String text = AppPaths.readInitialPassword(this);
        if (text == null || text.isEmpty()) {
            return false; // 网关还没首次启动过，等它写出文件
        }
        String username = credentialField(text, "username");
        String password = credentialField(text, "password");
        if (username.isEmpty() || password.isEmpty()) {
            return false;
        }
        String endpoint = "http://" + GatewayService.LISTEN_ADDR + "/admin/api/auth/login";
        HttpURLConnection connection = (HttpURLConnection) new URL(endpoint).openConnection();
        try {
            connection.setRequestMethod("POST");
            connection.setConnectTimeout(5000);
            connection.setReadTimeout(8000);
            connection.setDoOutput(true);
            connection.setRequestProperty("Content-Type", "application/json");
            // 面板要求写操作带这个同源标记（见 internal/panel 的校验）。
            connection.setRequestProperty("X-Admin-Request", "1");
            JSONObject payload = new JSONObject();
            payload.put("username", username);
            payload.put("password", password);
            try (OutputStream out = connection.getOutputStream()) {
                out.write(payload.toString().getBytes(StandardCharsets.UTF_8));
            }
            int code = connection.getResponseCode();
            if (code != 200) {
                Log.w(GatewayService.TAG, "自动登录 HTTP " + code);
                return false;
            }
            // 确认业务上也成功（HTTP 200 也可能带 ok:false）。
            String body = readAll(connection.getInputStream());
            JSONObject result = new JSONObject(body);
            if (!result.optBoolean("ok", false)) {
                Log.w(GatewayService.TAG, "自动登录被拒：" + result.optString("message"));
                return false;
            }
            String cookie = extractSessionCookie(connection);
            if (cookie == null) {
                Log.w(GatewayService.TAG, "自动登录成功但没有拿到会话 cookie");
                return false;
            }
            installCookie(cookie);
            return true;
        } finally {
            connection.disconnect();
        }
    }

    /** 从响应头里挑出会话 cookie（`wb2a_admin=…`），原样返回其 name=value 段。 */
    private static String extractSessionCookie(HttpURLConnection connection) {
        java.util.List<String> headers = connection.getHeaderFields().get("Set-Cookie");
        if (headers == null) {
            return null;
        }
        for (String header : headers) {
            if (header != null && header.startsWith("wb2a_admin=")) {
                int end = header.indexOf(';');
                return end > 0 ? header.substring(0, end) : header;
            }
        }
        return null;
    }

    /** 把会话 cookie 灌进 WebView 的 CookieManager（必须在 loadUrl 之前调用）。 */
    private void installCookie(String cookie) {
        CookieManager manager = CookieManager.getInstance();
        manager.setAcceptCookie(true);
        manager.setCookie("http://" + GatewayService.LISTEN_ADDR, cookie);
        // flush 让写入对随后的 loadUrl 立即可见（否则偶发竞态会先跳到登录页）。
        manager.flush();
    }

    private static String readAll(InputStream stream) throws Exception {
        StringBuilder text = new StringBuilder();
        try (BufferedReader reader = new BufferedReader(
                new InputStreamReader(stream, StandardCharsets.UTF_8))) {
            String line;
            while ((line = reader.readLine()) != null) {
                text.append(line);
            }
        }
        return text.toString();
    }

    private void refreshState() {
        GatewayService.portInUseAsync(running -> {
            if (running) {
                onGatewayReady();
            } else {
                showStarting();
                statusView.setText("网关未运行");
            }
        });
    }

    /**
     * 把面板的初始登录凭据显示给用户。
     *
     * ═══ 现在为什么还要留着它 ═══
     *
     * 正常路径下用户不再需要口令（App 自动登录，见 autoLogin）。但自动登录失败时
     * （凭据文件被改、面板口令被换过），用户仍然需要一条能进去的路——那时口令是
     * 他自己手机上一个他读不到的目录里的内容，只能由 App 显示。所以它从按钮变成了
     * **兜底弹窗**：只在首次进入、且还没有成功自动登录过时出现一次。
     *
     * ═══ 为什么账密分开复制 ═══
     *
     * 面板登录是两个输入框，用户得分别粘进「用户名」和「密码」。整段复制会带上
     * `username: ` / `password: ` 前缀和换行，粘进输入框就是脏值（还得手动删），
     * 而这恰恰是用户最容易卡住的一步。所以这里给两个独立的复制按钮，各自只复制值。
     */
    private void showCredentials() {
        String text = AppPaths.readInitialPassword(this);
        if (text == null || text.isEmpty()) {
            // 还没有说明网关没起来过——起来时会写这个文件。
            new AlertDialog.Builder(this)
                    .setTitle("还没有初始凭据")
                    .setMessage("网关尚未首次启动。启动后这里会显示面板的用户名与密码。")
                    .setPositiveButton("知道了", null)
                    .show();
            return;
        }
        String username = credentialField(text, "username");
        String password = credentialField(text, "password");
        if (username.isEmpty() || password.isEmpty()) {
            // 格式不符预期（用户手动改过文件？）：退回整段展示，至少不至于看不到。
            new AlertDialog.Builder(this)
                    .setTitle("面板登录凭据")
                    .setMessage(text + "\n\n登录后请在「系统」页改掉密码。")
                    .setPositiveButton("复制全部", (dialog, which) -> copyToClipboard(text))
                    .setNegativeButton("关闭", null)
                    .show();
            return;
        }

        LinearLayout body = new LinearLayout(this);
        body.setOrientation(LinearLayout.VERTICAL);
        body.setPadding(dp(20), dp(12), dp(20), dp(4));
        body.addView(credentialRow("用户名", username));
        body.addView(credentialRow("密码", password));

        TextView hint = new TextView(this);
        hint.setText("正常情况不用手动登录；若面板要求登录，请用上面的账号。登录后可在「系统」页改密码。");
        hint.setPadding(0, dp(10), 0, 0);
        body.addView(hint);

        new AlertDialog.Builder(this)
                .setTitle("面板登录凭据")
                .setView(body)
                .setPositiveButton("关闭", null)
                .show();
    }

    /** 一行凭据：左侧「标签 + 值」，右侧一个复制按钮。 */
    private View credentialRow(String label, String value) {
        LinearLayout row = new LinearLayout(this);
        row.setOrientation(LinearLayout.HORIZONTAL);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, dp(6), 0, dp(6));

        TextView text = new TextView(this);
        text.setText(label + "：" + value);
        text.setTextIsSelectable(true); // 长按也能手动选中
        row.addView(text, new LinearLayout.LayoutParams(
                0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f));

        Button copy = new Button(this);
        copy.setText("复制");
        copy.setOnClickListener(view -> copyToClipboard(value));
        row.addView(copy);
        return row;
    }

    /**
     * 从凭据文本里取出某个字段的值。
     *
     * 文件格式是网关写下的固定两行（`username: xxx` / `password: xxx`）。按行找前缀
     * 而不是按位置取：用户可能手动编辑过这个文件、加了注释或换了顺序。
     */
    private static String credentialField(String text, String key) {
        for (String line : text.split("\n")) {
            String trimmed = line.trim();
            if (trimmed.regionMatches(true, 0, key + ":", 0, key.length() + 1)) {
                return trimmed.substring(key.length() + 1).trim();
            }
        }
        return "";
    }

    private void copyToClipboard(String value) {
        android.content.ClipboardManager clipboard =
                (android.content.ClipboardManager) getSystemService(Context.CLIPBOARD_SERVICE);
        if (clipboard == null) {
            return;
        }
        clipboard.setPrimaryClip(android.content.ClipData.newPlainText("wb2a", value));
        toast("已复制");
    }

    /**
     * 引导用户把 App 加进电池优化白名单。
     *
     * 不加的话，Doze 模式会冻结前台服务，排程任务就不再按时跑——而用户完全看不出
     * 原因（界面上一切正常）。这个弹窗必须由用户确认，所以只能引导。
     */
    private void requestBatteryExemption() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) {
            return;
        }
        PowerManager manager = (PowerManager) getSystemService(Context.POWER_SERVICE);
        if (manager == null || manager.isIgnoringBatteryOptimizations(getPackageName())) {
            return;
        }
        try {
            Intent intent = new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS);
            intent.setData(Uri.parse("package:" + getPackageName()));
            startActivity(intent);
        } catch (Exception error) {
            // 部分厂商 ROM 没有这个 Activity：退回到「电池优化」设置列表页，
            // 让用户自己找。再失败就静默——不能因为引导失败就拦着不让启动。
            try {
                startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
            } catch (Exception ignored) {
                // 忽略
            }
        }
    }

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) {
            return;
        }
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                == PackageManager.PERMISSION_GRANTED) {
            return;
        }
        requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS},
                REQUEST_NOTIFICATIONS);
    }

    /**
     * 首次打开且网关已经跑起来时，主动把初始凭据弹给用户。
     *
     * ═══ 为什么现在仍然弹一次 ═══
     *
     * 自动登录让正常路径不再需要口令，但用户仍应知道这组凭据存在——换手机、清数据、
     * 在浏览器里开面板都要用它。所以保留「首次弹一次」，只是文案改成了「正常不用手填」。
     * 用 SharedPreferences 记住，不反复打扰。
     */
    private void maybeOfferCredentials() {
        android.content.SharedPreferences prefs =
                getSharedPreferences("wb2a", Context.MODE_PRIVATE);
        if (prefs.getBoolean("credentialsShown", false)) {
            return;
        }
        String text = AppPaths.readInitialPassword(this);
        if (text == null || text.isEmpty()) {
            return; // 网关还没首次启动，等它写出文件后再说
        }
        prefs.edit().putBoolean("credentialsShown", true).apply();
        showCredentials();
    }

    /**
     * 有待安装的更新包时，**立刻弹对话框问是否安装**。
     *
     * ═══ 为什么从「按钮」改成「对话框」 ═══
     *
     * 初版是下载完在顶栏显示一个「安装更新」按钮，等用户退回首页点它。问题是用户
     * 那时候通常正在面板里看别的东西（甚至不在这个 App 里），这条按钮就成了一个
     * 「你得先回去找到它」的额外步骤，更新流程被硬生生拆成两段。
     *
     * 改成对话框后：下载完 → 弹窗 → 点「安装」直接进系统安装器。
     *
     * ═══ 为什么点「安装」这个动作仍然必须由用户发出 ═══
     *
     * 起系统安装器这件事，试过两条路都不行：
     *   - Service 里 startActivity：Android 10+ 禁止后台启动 Activity，被静默丢弃；
     *   - Activity 的 onResume 里 startActivity：国产 ROM（实测 vivo）同样拦截，
     *     连异常都不抛——用户看到的是「点了更新但什么都没发生」。
     *
     * 而**用户手势**触发的 startActivity 任何 ROM 都不会拦（这是系统明确的豁免）。
     * 对话框的「安装」按钮正是这样一个手势，所以现在既不用退回首页、也不违反限制。
     */
    private void promptInstallIfPending() {
        File pending = GatewayService.pendingInstall(this);
        if (pending == null || isFinishing()) {
            return;
        }
        long stamp = pending.length() * 31 + pending.lastModified();
        if (stamp == promptedStamp || stamp == installedStamp) {
            return; // 这一份已经问过了
        }
        // 安装成功后 pending-install.apk 会**留在原地**（系统安装器不会替我们删，
        // 而应用私有目录里的文件我们也没法在安装过程中改）。不校验的话，下次启动
        // 会拿这份已经装过的包反复弹「发现新版本」——用户点了安装却什么都没发生，
        // 而且永远弹不完。所以先比 versionCode：不比当前版本新就说明装过了。
        if (!isNewerThanInstalled(pending)) {
            Log.i(GatewayService.TAG, "待安装包不比当前版本新，清理：" + pending.getName());
            //noinspection ResultOfMethodCallIgnored
            pending.delete();
            return;
        }
        promptedStamp = stamp;
        new AlertDialog.Builder(this)
                .setTitle("发现新版本")
                .setMessage("更新包已下载并校验完成，现在安装？\n\n安装完成后应用数据（账号、密钥、配置）都会保留。")
                .setCancelable(true)
                .setPositiveButton("安装", (dialog, which) -> installPending(pending))
                .setNegativeButton("稍后", null)
                .show();
    }

    /**
     * 待安装的 APK 是否比当前运行的版本新。
     *
     * 用 `getPackageArchiveInfo` 读那个**尚未安装**的 APK 的清单，取它的 versionCode
     * 与当前应用比。安卓判断新旧只看 versionCode（这正是 build.gradle.kts 里
     * versionCode 必须递增的原因），所以这里的判据与系统的判据是同一个。
     *
     * 读不出来（文件损坏、被截断）时返回 false：宁可不弹，也不要拉一个装不上的包
     * 去让系统安装器报错。
     */
    private boolean isNewerThanInstalled(File apk) {
        try {
            android.content.pm.PackageInfo info = getPackageManager()
                    .getPackageArchiveInfo(apk.getAbsolutePath(), 0);
            if (info == null) {
                return false;
            }
            long pendingCode = Build.VERSION.SDK_INT >= Build.VERSION_CODES.P
                    ? info.getLongVersionCode()
                    : info.versionCode;
            android.content.pm.PackageInfo current = getPackageManager()
                    .getPackageInfo(getPackageName(), 0);
            long currentCode = Build.VERSION.SDK_INT >= Build.VERSION_CODES.P
                    ? current.getLongVersionCode()
                    : current.versionCode;
            return pendingCode > currentCode;
        } catch (Exception error) {
            Log.w(GatewayService.TAG, "读取待安装包版本失败", error);
            return false;
        }
    }

    private void installPending(File pending) {
        installedStamp = pending.length() * 31 + pending.lastModified();
        // 用户手势触发的 startActivity：任何 ROM 都不会拦。
        String message = GatewayService.launchInstaller(this, pending);
        toast(message);
    }

    private void toast(String message) {
        Toast.makeText(this, message, Toast.LENGTH_SHORT).show();
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}
