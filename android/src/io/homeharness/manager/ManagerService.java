package io.homeharness.manager;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.wifi.WifiManager;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.os.SystemClock;
import android.util.Log;

import java.io.File;

/**
 * Keeps the manager running: a foreground service that starts the bundled
 * manager binary and restarts it (with backoff) whenever it exits, until
 * the service is stopped. Optionally holds a partial wake lock and a
 * Wi-Fi lock so the phone keeps serving with the screen off.
 */
public class ManagerService extends Service {
    private static final String TAG = "HomeHarness";
    private static final String CHANNEL = "manager";
    private static final long MAX_LOG = 4L << 20;

    static volatile boolean running;

    // Guards stopping + process together, so a stop can never fall between
    // the supervisor's check and its start of a new manager (which would
    // then outlive the service — and an import would write under it).
    private final Object lock = new Object();
    private volatile boolean stopping;
    private Thread supervisor;
    private Process process;
    private PowerManager.WakeLock wakeLock;
    private WifiManager.WifiLock wifiLock;
    private WifiManager.MulticastLock multicastLock;

    static void start(Context c) {
        Intent i = new Intent(c, ManagerService.class);
        if (Build.VERSION.SDK_INT >= 26) c.startForegroundService(i);
        else c.startService(i);
    }

    static void stop(Context c) {
        c.stopService(new Intent(c, ManagerService.class));
    }

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CHANNEL, "Manager", NotificationManager.IMPORTANCE_LOW));
        Notification n = notification("Starting…");
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(1, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(1, n);
        }
        running = true;
    }

    private Notification notification(String text) {
        PendingIntent open = PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class),
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        return new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.drawable.ic_launcher)
                .setContentTitle("Home Harness manager")
                .setContentText(text)
                .setContentIntent(open)
                .setOngoing(true)
                .build();
    }

    private void updateNotification(String text) {
        getSystemService(NotificationManager.class).notify(1, notification(text));
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (supervisor == null) {
            acquireLocks();
            supervisor = new Thread(this::supervise, "manager-supervisor");
            supervisor.start();
        }
        return START_STICKY;
    }

    private void acquireLocks() {
        WifiManager wm = (WifiManager) getApplicationContext().getSystemService(Context.WIFI_SERVICE);
        if (wm != null) {
            multicastLock = wm.createMulticastLock("harness-beacon");
            multicastLock.setReferenceCounted(false);
            multicastLock.acquire();
        }
        if (!Harness.prefs(this).getBoolean(Harness.KEY_KEEP_AWAKE, true)) return;
        PowerManager pm = getSystemService(PowerManager.class);
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "HomeHarness:manager");
        wakeLock.setReferenceCounted(false);
        wakeLock.acquire();
        if (wm != null) {
            wifiLock = wm.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "HomeHarness:manager");
            wifiLock.setReferenceCounted(false);
            wifiLock.acquire();
        }
    }

    private void supervise() {
        long backoff = 2000;
        while (!stopping) {
            long started = SystemClock.elapsedRealtime();
            try {
                Harness.killStale(this);
                Harness.copyAgents(this);
                File log = Harness.logFile(this);
                log.getParentFile().mkdirs();
                if (log.length() > MAX_LOG) {
                    File old = new File(log.getPath() + ".1");
                    old.delete();
                    log.renameTo(old);
                }
                ProcessBuilder pb = new ProcessBuilder(Harness.command(this))
                        .directory(Harness.stateDir(this))
                        .redirectErrorStream(true)
                        .redirectOutput(ProcessBuilder.Redirect.appendTo(log));
                String ip = Harness.lanAddress(this);
                updateNotification(ip != null ? "Devices join at " + ip + ":" + Harness.AGENT_PORT : "Running (no network)");
                Process p;
                synchronized (lock) {
                    if (stopping) break;
                    p = pb.start();
                    process = p;
                }
                int code = p.waitFor();
                Log.w(TAG, "manager exited with " + code);
            } catch (InterruptedException e) {
                break;
            } catch (Exception e) {
                Log.e(TAG, "manager start failed", e);
            }
            if (stopping) break;
            if (SystemClock.elapsedRealtime() - started > 60_000) backoff = 2000;
            updateNotification("Restarting in " + backoff / 1000 + "s");
            try {
                Thread.sleep(backoff);
            } catch (InterruptedException e) {
                break;
            }
            backoff = Math.min(backoff * 2, 60_000);
        }
    }

    @Override
    public void onDestroy() {
        Process p;
        synchronized (lock) {
            stopping = true;
            p = process;
        }
        if (p != null) p.destroy();
        if (supervisor != null) supervisor.interrupt();
        if (wakeLock != null && wakeLock.isHeld()) wakeLock.release();
        if (wifiLock != null && wifiLock.isHeld()) wifiLock.release();
        if (multicastLock != null && multicastLock.isHeld()) multicastLock.release();
        running = false;
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
