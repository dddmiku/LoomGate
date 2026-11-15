// ═══ 更新日志 ═══
// 2026-10-13：初版——把更新下来的 APK 以 content:// 交给系统安装器。
//
// ═══ 为什么需要它 ═══
//
// Android 7 起，把 file:// URI 通过 Intent 传给**另一个进程**会抛
// FileUriExposedException，而 packageinstaller 正是另一个进程。官方解法是
// FileProvider，但那个类在 androidx.core 里——这个 App 刻意不引 androidx
// （全部界面就是一个 WebView，不需要那套框架）。
//
// 所以这里实现一个最小的等价物：只服务一个文件（更新下来的 APK），只支持读，
// 且通过 grantUriPermissions 按次授权，不对外暴露目录。
//
// 安全性：它只暴露 <cache>/install/ 下的文件，而那个目录里只有我们自己复制过去
// 的发行版 APK——不含任何用户数据。没有路径穿越面（用文件名比对，不拼路径）。
package com.workbuddy2api.app;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.ParcelFileDescriptor;
import android.provider.OpenableColumns;

import java.io.File;
import java.io.FileNotFoundException;

public class ApkProvider extends ContentProvider {
    /** AUTHORITY 与 AndroidManifest 里的声明一致。 */
    public static final String AUTHORITY = "com.workbuddy2api.app.apk";

    /** 只服务这个目录下的文件。 */
    private File installDir() {
        File external = getContext() == null ? null : getContext().getExternalCacheDir();
        File base = external != null ? external : getContext().getCacheDir();
        return new File(base, "install");
    }

    @Override
    public boolean onCreate() {
        return true;
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws FileNotFoundException {
        String name = uri.getLastPathSegment();
        if (name == null || name.isEmpty()) {
            throw new FileNotFoundException("缺少文件名");
        }
        // 只允许直接子文件：名字里出现分隔符一律拒绝，避免被拼出目录外的路径。
        if (name.contains("/") || name.contains("\\") || name.contains("..")) {
            throw new FileNotFoundException("文件名非法");
        }
        File target = new File(installDir(), name);
        if (!target.isFile()) {
            throw new FileNotFoundException("文件不存在");
        }
        return ParcelFileDescriptor.open(target, ParcelFileDescriptor.MODE_READ_ONLY);
    }

    @Override
    public String getType(Uri uri) {
        return "application/vnd.android.package-archive";
    }

    /**
     * 系统安装器**必须**能从这里问到文件名与大小。
     *
     * 返回 null 会让它在 staging 阶段失败，报「解析软件包时出现问题」——
     * 2026-10-13 真机实测：APK 本身完全正常（adb install 能装），只是安装器
     * 拿不到 OpenableColumns 里的 DISPLAY_NAME / SIZE，于是没能把内容复制出去。
     */
    @Override
    public Cursor query(Uri uri, String[] projection, String selection, String[] selectionArgs,
                        String sortOrder) {
        String name = uri.getLastPathSegment();
        if (name == null || name.isEmpty()) {
            return null;
        }
        File target = new File(installDir(), name);
        if (!target.isFile()) {
            return null;
        }
        android.database.MatrixCursor cursor = new android.database.MatrixCursor(
                new String[]{android.provider.OpenableColumns.DISPLAY_NAME,
                        android.provider.OpenableColumns.SIZE});
        cursor.addRow(new Object[]{target.getName(), target.length()});
        return cursor;
    }

    @Override
    public Uri insert(Uri uri, ContentValues values) {
        return null;
    }

    @Override
    public int delete(Uri uri, String selection, String[] selectionArgs) {
        return 0;
    }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) {
        return 0;
    }
}
