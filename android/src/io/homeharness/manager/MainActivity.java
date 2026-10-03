package io.homeharness.manager;

import android.app.Activity;
import android.app.AlertDialog;
import android.content.ContentValues;
import android.content.Intent;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.os.PowerManager;
import android.provider.DocumentsContract;
import android.provider.MediaStore;
import android.provider.Settings;
import android.view.Menu;
import android.view.MenuItem;
import android.webkit.JavascriptInterface;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Toast;

import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;

/**
 * The manager's own dashboard in a WebView, locked to the local manager
 * (http://127.0.0.1:7421). Signs in with the operator token the manager
 * keeps in the app's private files — nothing to type.
 */
public class MainActivity extends Activity {
    private static final String BASE = "http://127.0.0.1:" + Harness.API_PORT;
    private static final int PICK_FILES = 1, IMPORT_STATE = 2, EXPORT_STATE = 3;
    private static final int M_RELOAD = 1, M_IMPORT = 2, M_EXPORT = 3, M_AWAKE = 4, M_BOOT = 5, M_BATTERY = 6, M_TOGGLE = 7, M_LOG = 8;

    private WebView web;
    private ValueCallback<Uri[]> fileCallback;

    @Override
    protected void onCreate(Bundle saved) {
        super.onCreate(saved);
        web = new WebView(this);
        setContentView(web);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true); // the dashboard keeps its session in localStorage
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(false);
        web.addJavascriptInterface(new Bridge(), "HarnessAndroid");
        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                if ("127.0.0.1".equals(u.getHost()) && u.getPort() == Harness.API_PORT) {
                    // Same origin: load it only once the server proves it
                    // is our manager (the page would get localStorage).
                    if (!req.isForMainFrame()) return false;
                    load(u.toString());
                    return true;
                }
                // Anything else (e.g. an enrollment link) opens outside,
                // so no other page ever runs next to the bridge.
                startActivity(new Intent(Intent.ACTION_VIEW, u));
                return true;
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> callback, FileChooserParams params) {
                if (fileCallback != null) fileCallback.onReceiveValue(null);
                fileCallback = callback;
                Intent i = new Intent(Intent.ACTION_GET_CONTENT);
                i.addCategory(Intent.CATEGORY_OPENABLE);
                i.setType("*/*");
                i.putExtra(Intent.EXTRA_ALLOW_MULTIPLE, params.getMode() == FileChooserParams.MODE_OPEN_MULTIPLE);
                startActivityForResult(Intent.createChooser(i, "Choose files"), PICK_FILES);
                return true;
            }
        });
        if (Build.VERSION.SDK_INT >= 33) requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 9);
        ManagerService.start(this);
        askBatteryOnce();
        waitAndLoad();
    }

    private void showPage(String title, String body) {
        String html = "<html><body style='font-family:sans-serif;padding:24px'><h3>" + escape(title) + "</h3><pre style='white-space:pre-wrap;font-size:12px'>"
                + escape(body) + "</pre></body></html>";
        web.loadDataWithBaseURL(null, html, "text/html", "utf-8", null);
    }

    private static String escape(String s) {
        return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;");
    }

    /** Shows "starting" until the manager's API answers, then signs in. */
    private void waitAndLoad() {
        showPage("Starting the manager…", "");
        load(null);
    }

    /** Loads url (null: the sign-in link) once 127.0.0.1:7421 proves it
     *  is this app's manager, waiting up to a minute for it to come up. */
    private void load(String url) {
        new Thread(() -> {
            Harness.Server last = Harness.Server.DOWN;
            for (int i = 0; i < 120; i++) {
                last = Harness.checkServer(this);
                if (last == Harness.Server.OURS) {
                    String u = url != null ? url : BASE + "/#login=" + Harness.operatorToken(this);
                    runOnUiThread(() -> web.loadUrl(u));
                    return;
                }
                try {
                    Thread.sleep(500);
                } catch (InterruptedException e) {
                    return;
                }
            }
            String tail = Harness.logTail(this, 4000);
            String why = last == Harness.Server.IMPOSTOR
                    ? "Something that is not this app's manager is answering on 127.0.0.1:" + Harness.API_PORT
                            + " (another app may be using the port), so the app is not signing in to it.\n\n"
                    : "If the Termux manager is still running on this phone, stop it (port 7420 is taken).\n\n";
            runOnUiThread(() -> showPage("The manager didn't start", why + "Manager log:\n" + tail));
        }).start();
    }

    private void askBatteryOnce() {
        if (Harness.prefs(this).getBoolean(Harness.KEY_ASKED_BATTERY, false)) return;
        Harness.prefs(this).edit().putBoolean(Harness.KEY_ASKED_BATTERY, true).apply();
        PowerManager pm = getSystemService(PowerManager.class);
        if (pm.isIgnoringBatteryOptimizations(getPackageName())) return;
        new AlertDialog.Builder(this)
                .setTitle("Keep the manager running")
                .setMessage("Your devices connect to this phone. Allow Home Harness to run in the background without battery restrictions, or Android may stop it.")
                .setPositiveButton("Allow", (d, w) -> openBatterySettings())
                .setNegativeButton("Later", null)
                .show();
    }

    private void openBatterySettings() {
        try {
            startActivity(new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:" + getPackageName())));
        } catch (Exception e) {
            startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
        }
    }

    // ---- downloads: the WebView can't save the dashboard's blobs, so the
    // app fetches the file itself with the operator token it already has.
    final class Bridge {
        @JavascriptInterface
        public void download(String sha, String name) {
            if (sha == null || !sha.matches("[0-9a-f]{64}")) return;
            String base = name == null ? "" : name.substring(name.lastIndexOf('/') + 1);
            final String file = base.matches("[A-Za-z0-9._-]{1,128}") ? base : sha + ".bin";
            new Thread(() -> {
                String msg;
                try {
                    msg = "Saved " + save(sha, file);
                } catch (IOException e) {
                    msg = "Download failed: " + e.getMessage();
                }
                final String m = msg;
                runOnUiThread(() -> Toast.makeText(MainActivity.this, m, Toast.LENGTH_LONG).show());
            }).start();
        }
    }

    private String save(String sha, String name) throws IOException {
        if (Harness.checkServer(this) != Harness.Server.OURS) {
            throw new IOException("the manager isn't answering (or something else is on its port)");
        }
        HttpURLConnection c = (HttpURLConnection) new URL(BASE + "/artifacts/" + sha).openConnection();
        c.setRequestProperty("Authorization", "Bearer " + Harness.operatorToken(this));
        if (c.getResponseCode() != 200) throw new IOException("manager answered " + c.getResponseCode());
        try (InputStream in = c.getInputStream()) {
            if (Build.VERSION.SDK_INT >= 29) {
                ContentValues v = new ContentValues();
                v.put(MediaStore.Downloads.DISPLAY_NAME, name);
                v.put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/HomeHarness");
                Uri uri = getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
                if (uri == null) throw new IOException("can't create the file");
                try (OutputStream out = getContentResolver().openOutputStream(uri)) {
                    copy(in, out);
                }
                return "to Downloads/HomeHarness/" + name;
            }
            File dir = getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS);
            File f = new File(dir, name);
            try (OutputStream out = new FileOutputStream(f)) {
                copy(in, out);
            }
            return "to " + f.getAbsolutePath();
        } finally {
            c.disconnect();
        }
    }

    private static void copy(InputStream in, OutputStream out) throws IOException {
        byte[] buf = new byte[1 << 16];
        int r;
        while ((r = in.read(buf)) > 0) out.write(buf, 0, r);
    }

    // ---- menu

    @Override
    public boolean onCreateOptionsMenu(Menu menu) {
        menu.add(0, M_RELOAD, 0, "Reload");
        menu.add(0, M_IMPORT, 0, "Import state (from Termux)…");
        menu.add(0, M_EXPORT, 0, "Export state…");
        menu.add(0, M_AWAKE, 0, "Keep Wi-Fi and CPU awake").setCheckable(true)
                .setChecked(Harness.prefs(this).getBoolean(Harness.KEY_KEEP_AWAKE, true));
        menu.add(0, M_BOOT, 0, "Start when the phone boots").setCheckable(true)
                .setChecked(Harness.prefs(this).getBoolean(Harness.KEY_START_ON_BOOT, true));
        menu.add(0, M_BATTERY, 0, "Battery optimization…");
        menu.add(0, M_TOGGLE, 0, ManagerService.running ? "Stop the manager" : "Start the manager");
        menu.add(0, M_LOG, 0, "Manager log");
        return true;
    }

    @Override
    public boolean onPrepareOptionsMenu(Menu menu) {
        menu.findItem(M_TOGGLE).setTitle(ManagerService.running ? "Stop the manager" : "Start the manager");
        return true;
    }

    @Override
    public boolean onOptionsItemSelected(MenuItem item) {
        switch (item.getItemId()) {
            case M_RELOAD:
                waitAndLoad();
                return true;
            case M_IMPORT: {
                Intent i = new Intent(Intent.ACTION_OPEN_DOCUMENT);
                i.addCategory(Intent.CATEGORY_OPENABLE);
                i.setType("*/*");
                startActivityForResult(i, IMPORT_STATE);
                return true;
            }
            case M_EXPORT: {
                Intent i = new Intent(Intent.ACTION_CREATE_DOCUMENT);
                i.addCategory(Intent.CATEGORY_OPENABLE);
                i.setType("application/zip");
                i.putExtra(Intent.EXTRA_TITLE, "home-harness-state.zip");
                startActivityForResult(i, EXPORT_STATE);
                return true;
            }
            case M_AWAKE:
            case M_BOOT: {
                String key = item.getItemId() == M_AWAKE ? Harness.KEY_KEEP_AWAKE : Harness.KEY_START_ON_BOOT;
                boolean on = !item.isChecked();
                item.setChecked(on);
                Harness.prefs(this).edit().putBoolean(key, on).apply();
                if (item.getItemId() == M_AWAKE) restartManager();
                return true;
            }
            case M_BATTERY:
                openBatterySettings();
                return true;
            case M_TOGGLE:
                if (ManagerService.running) {
                    ManagerService.stop(this);
                    showPage("Manager stopped", "Devices can't reach this phone until you start it again (menu).");
                } else {
                    ManagerService.start(this);
                    waitAndLoad();
                }
                return true;
            case M_LOG:
                new AlertDialog.Builder(this).setTitle("Manager log").setMessage(Harness.logTail(this, 6000))
                        .setPositiveButton("OK", null).show();
                return true;
        }
        return super.onOptionsItemSelected(item);
    }

    private void restartManager() {
        ManagerService.stop(this);
        ManagerService.start(this);
        waitAndLoad();
    }

    /** Stops the manager and waits until no manager process is left (an
     *  import must never write under a running manager's database). */
    private void stopAndWait() throws InterruptedException, IOException {
        ManagerService.stop(this);
        for (int i = 0; i < 40 && ManagerService.running; i++) Thread.sleep(250);
        // A few seconds to shut down cleanly, then killed.
        for (int i = 0; i < 20 && Harness.managerAlive(this); i++) Thread.sleep(250);
        Harness.killStale(this);
        for (int i = 0; i < 20 && Harness.managerAlive(this); i++) Thread.sleep(250);
        if (Harness.managerAlive(this)) throw new IOException("the manager didn't stop; try again");
    }

    @Override
    protected void onActivityResult(int request, int result, Intent data) {
        super.onActivityResult(request, result, data);
        if (request == PICK_FILES) {
            Uri[] picked = null;
            if (result == RESULT_OK && data != null) {
                if (data.getClipData() != null) {
                    picked = new Uri[data.getClipData().getItemCount()];
                    for (int i = 0; i < picked.length; i++) picked[i] = data.getClipData().getItemAt(i).getUri();
                } else if (data.getData() != null) {
                    picked = new Uri[]{data.getData()};
                }
            }
            if (fileCallback != null) fileCallback.onReceiveValue(picked);
            fileCallback = null;
            return;
        }
        if (result != RESULT_OK || data == null || data.getData() == null) return;
        Uri uri = data.getData();
        if (request == IMPORT_STATE) importState(uri);
        if (request == EXPORT_STATE) exportState(uri);
    }

    private void importState(Uri uri) {
        showPage("Importing…", "");
        new Thread(() -> {
            String msg;
            boolean ok = false;
            try {
                File tmp = new File(getCacheDir(), "import.zip");
                try (InputStream in = getContentResolver().openInputStream(uri); OutputStream out = new FileOutputStream(tmp)) {
                    copy(in, out);
                }
                stopAndWait();
                msg = Harness.runOnce(this, "-import-state", tmp.getAbsolutePath(), "-state-dir", Harness.stateDir(this).getAbsolutePath(), "-force");
                tmp.delete();
                ok = true;
            } catch (Exception e) {
                msg = "Import failed: " + e.getMessage();
            }
            ManagerService.start(this);
            final String m = msg;
            final boolean imported = ok;
            runOnUiThread(() -> {
                waitAndLoad();
                AlertDialog.Builder b = new AlertDialog.Builder(this).setTitle(imported ? "State imported" : "Import failed").setMessage(m);
                if (imported) {
                    // The export holds the manager's TLS key and tokens.
                    b.setMessage(m + "\n\nThe file you imported holds this manager's keys. Delete it now?")
                            .setPositiveButton("Delete it", (d, w) -> {
                                try {
                                    DocumentsContract.deleteDocument(getContentResolver(), uri);
                                    Toast.makeText(this, "Deleted", Toast.LENGTH_SHORT).show();
                                } catch (Exception e) {
                                    Toast.makeText(this, "Couldn't delete it; please delete it yourself", Toast.LENGTH_LONG).show();
                                }
                            })
                            .setNegativeButton("Keep", null);
                } else {
                    b.setPositiveButton("OK", null);
                }
                b.show();
            });
        }).start();
    }

    private void exportState(Uri uri) {
        showPage("Exporting…", "");
        new Thread(() -> {
            String msg;
            try {
                File tmp = new File(getCacheDir(), "export.zip");
                tmp.delete();
                stopAndWait();
                Harness.runOnce(this, "-export-state", tmp.getAbsolutePath(), "-state-dir", Harness.stateDir(this).getAbsolutePath());
                try (InputStream in = new FileInputStream(tmp); OutputStream out = getContentResolver().openOutputStream(uri)) {
                    copy(in, out);
                }
                tmp.delete();
                msg = "Exported. The file holds this manager's TLS key and tokens: keep it private.";
            } catch (Exception e) {
                msg = "Export failed: " + e.getMessage();
            }
            ManagerService.start(this);
            final String m = msg;
            runOnUiThread(() -> {
                waitAndLoad();
                new AlertDialog.Builder(this).setTitle("Export").setMessage(m).setPositiveButton("OK", null).show();
            });
        }).start();
    }

    @Override
    public void onBackPressed() {
        if (web.canGoBack()) web.goBack();
        else super.onBackPressed();
    }
}
