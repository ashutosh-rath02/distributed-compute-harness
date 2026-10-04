package io.homeharness.manager;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageInfo;
import android.content.pm.PackageInstaller;
import android.os.Build;
import android.util.Log;

import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.security.MessageDigest;

/**
 * Keeps the app up to date from its manager (worker mode). The agent is
 * part of this app and can't replace itself: when the manager offers a
 * newer app, the agent downloads it — checked against the hash the manager
 * named — to updateFile(), and this installs it with PackageInstaller.
 * Android asks the owner once (and to allow installing from this app);
 * after this app has installed itself, Android 12+ lets later updates
 * through without asking. Android then restarts the app with
 * MY_PACKAGE_REPLACED (BootReceiver), which starts the worker again.
 */
final class AppUpdater {
    private static final String TAG = "HomeHarness";
    private static final String CHANNEL = "updates";
    private static final int NOTIFICATION = 2;

    /** What the worker screen shows about updates ("" = nothing). */
    static volatile String status = "";
    /** Android's "install this update?" screen, while it waits for the owner. */
    static volatile Intent confirm;
    /** Hand the download to the installer again (retryAfterAbort). */
    private static volatile boolean resubmit;
    /** The download retried after an abort (its time): retried once only. */
    private static volatile long retriedFor;

    static File updateFile(Context c) {
        return new File(new File(Harness.workerDir(c), "update"), "home-harness.apk");
    }

    private final Context c;
    private volatile boolean stopped;
    private Thread thread;
    /** The download last handed to the installer (its time), so it is handed over once. */
    private long submitted;

    AppUpdater(Context c) {
        this.c = c.getApplicationContext();
    }

    void start() {
        thread = new Thread(this::loop, "app-updater");
        thread.start();
    }

    void stop() {
        stopped = true;
        if (thread != null) thread.interrupt();
    }

    private void loop() {
        while (!stopped) {
            try {
                check();
            } catch (Exception e) {
                Log.e(TAG, "app update", e);
                fail(c, "Couldn't install the update: " + e.getMessage());
            }
            try {
                Thread.sleep(5000);
            } catch (InterruptedException e) {
                return;
            }
        }
    }

    private void check() throws Exception {
        if (resubmit) {
            resubmit = false;
            submitted = 0;
        }
        File f = updateFile(c);
        if (!f.isFile() || f.lastModified() == submitted) return;
        // The update this app now runs (it was restarted after installing it).
        if (sha256(f).equals(sha256(new File(c.getApplicationInfo().sourceDir)))) {
            f.delete();
            status = "";
            return;
        }
        PackageInfo info = c.getPackageManager().getPackageArchiveInfo(f.getPath(), 0);
        if (info == null || !c.getPackageName().equals(info.packageName)) {
            fail(c, "The downloaded update isn't this app.");
            return;
        }
        submitted = f.lastModified();
        status = "Installing an update from the manager…";
        install(f);
    }

    private void install(File f) throws IOException {
        PackageInstaller installer = c.getPackageManager().getPackageInstaller();
        PackageInstaller.SessionParams p = new PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL);
        p.setAppPackageName(c.getPackageName());
        if (Build.VERSION.SDK_INT >= 31) p.setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED);
        int id = installer.createSession(p);
        try (PackageInstaller.Session s = installer.openSession(id)) {
            try (InputStream in = new FileInputStream(f); OutputStream out = s.openWrite("base.apk", 0, f.length())) {
                byte[] buf = new byte[1 << 16];
                int r;
                while ((r = in.read(buf)) > 0) out.write(buf, 0, r);
                s.fsync(out);
            }
            // Explicit, and mutable: the installer adds the outcome.
            Intent done = new Intent(c, InstallReceiver.class);
            PendingIntent result = PendingIntent.getBroadcast(c, id, done, PendingIntent.FLAG_MUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
            s.commit(result.getIntentSender());
        }
        Log.i(TAG, "app update handed to the installer (session " + id + ")");
    }

    /** Android wants the owner to confirm: a notification to tap (a
     *  background service can't open the screen itself), and the worker
     *  screen's Install button. */
    static void askToConfirm(Context c, Intent screen) {
        screen.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        confirm = screen;
        status = "An update from the manager is ready. Tap Install.";
        PendingIntent open = PendingIntent.getActivity(c, 3, screen, PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        notify(c, "Update Home Harness", "A newer version from your manager is ready. Tap to install it.", open);
    }

    /** An install canceled while the owner was allowing this app to install
     *  updates (Android 15 cancels it when they go to Settings from its
     *  prompt): now that it may, install the same download again — once,
     *  so an owner who really says no isn't asked over and over. */
    static boolean retryAfterAbort(Context c) {
        File f = updateFile(c);
        if (!f.isFile() || !c.getPackageManager().canRequestPackageInstalls() || retriedFor == f.lastModified()) return false;
        retriedFor = f.lastModified();
        confirm = null;
        status = "Installing an update from the manager…";
        resubmit = true;
        return true;
    }

    static void installed(Context c) {
        confirm = null;
        status = "";
        cancelNotification(c);
    }

    /** The update didn't install: say why, and drop the download (the
     *  manager offers it again on the next update). */
    static void fail(Context c, String why) {
        Log.w(TAG, why);
        confirm = null;
        status = why;
        updateFile(c).delete();
        PendingIntent open = PendingIntent.getActivity(c, 4, new Intent(c, MainActivity.class), PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        notify(c, "Home Harness update", why, open);
    }

    private static void notify(Context c, String title, String text, PendingIntent tap) {
        NotificationManager nm = c.getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL, "Updates", NotificationManager.IMPORTANCE_DEFAULT));
        nm.notify(NOTIFICATION, new Notification.Builder(c, CHANNEL)
                .setSmallIcon(R.drawable.ic_launcher)
                .setContentTitle(title)
                .setContentText(text)
                .setContentIntent(tap)
                .setAutoCancel(true)
                .build());
    }

    private static void cancelNotification(Context c) {
        c.getSystemService(NotificationManager.class).cancel(NOTIFICATION);
    }

    static String sha256(File f) throws Exception {
        MessageDigest md = MessageDigest.getInstance("SHA-256");
        try (InputStream in = new FileInputStream(f)) {
            byte[] buf = new byte[1 << 16];
            int r;
            while ((r = in.read(buf)) > 0) md.update(buf, 0, r);
        }
        return Harness.hex(md.digest());
    }
}
