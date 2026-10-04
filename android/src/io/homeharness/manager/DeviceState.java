package io.homeharness.manager;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.os.BatteryManager;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.util.Log;

import java.io.File;
import java.io.FileOutputStream;
import java.nio.charset.StandardCharsets;

/**
 * Keeps the worker's device-state file current with the phone's charging
 * and screen state, for the agent's reports to the manager (which hold
 * new work back while the phone is unplugged, by default). The agent is a
 * plain child process and can't ask Android itself. The file is rewritten
 * on every change and every 30 seconds, so the agent can tell a live
 * writer from one that stopped (it ignores a file older than 2 minutes).
 */
final class DeviceState {
    private static final String TAG = "HomeHarness";
    private static final long EVERY_MS = 30_000;

    private final Context context;
    private final File file;
    private final Handler handler = new Handler(Looper.getMainLooper());
    /** Unix seconds when the screen went off; 0 while it is on. */
    private volatile long screenOffSince;

    private final BroadcastReceiver receiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context c, Intent intent) {
            String action = intent.getAction();
            if (Intent.ACTION_SCREEN_OFF.equals(action)) {
                screenOffSince = System.currentTimeMillis() / 1000;
            } else if (Intent.ACTION_SCREEN_ON.equals(action)) {
                screenOffSince = 0;
            }
            write();
        }
    };

    private final Runnable tick = new Runnable() {
        @Override
        public void run() {
            write();
            handler.postDelayed(this, EVERY_MS);
        }
    };

    DeviceState(Context context, File file) {
        this.context = context.getApplicationContext();
        this.file = file;
    }

    void start() {
        PowerManager pm = context.getSystemService(PowerManager.class);
        if (pm != null && !pm.isInteractive()) screenOffSince = System.currentTimeMillis() / 1000;
        IntentFilter f = new IntentFilter();
        f.addAction(Intent.ACTION_SCREEN_ON);
        f.addAction(Intent.ACTION_SCREEN_OFF);
        f.addAction(Intent.ACTION_POWER_CONNECTED);
        f.addAction(Intent.ACTION_POWER_DISCONNECTED);
        f.addAction(Intent.ACTION_BATTERY_CHANGED);
        // System broadcasts only: they still reach a not-exported receiver.
        if (Build.VERSION.SDK_INT >= 33) {
            context.registerReceiver(receiver, f, Context.RECEIVER_NOT_EXPORTED);
        } else {
            context.registerReceiver(receiver, f);
        }
        handler.post(tick);
    }

    void stop() {
        handler.removeCallbacks(tick);
        try {
            context.unregisterReceiver(receiver);
        } catch (IllegalArgumentException ignored) {
            // never registered
        }
    }

    private void write() {
        // The battery state is a sticky broadcast: reading it needs no receiver.
        Intent battery = context.registerReceiver(null, new IntentFilter(Intent.ACTION_BATTERY_CHANGED));
        StringBuilder s = new StringBuilder();
        s.append("updated=").append(System.currentTimeMillis() / 1000).append('\n');
        if (battery != null) {
            int plugged = battery.getIntExtra(BatteryManager.EXTRA_PLUGGED, -1);
            if (plugged >= 0) s.append("on_battery=").append(plugged == 0 ? 1 : 0).append('\n');
            int level = battery.getIntExtra(BatteryManager.EXTRA_LEVEL, -1);
            int scale = battery.getIntExtra(BatteryManager.EXTRA_SCALE, -1);
            if (level >= 0 && scale > 0) s.append("battery=").append(level * 100 / scale).append('\n');
        }
        long off = screenOffSince;
        s.append("screen_on=").append(off == 0 ? 1 : 0).append('\n');
        if (off != 0) s.append("screen_off_since=").append(off).append('\n');
        File tmp = new File(file.getPath() + ".tmp");
        try (FileOutputStream out = new FileOutputStream(tmp)) {
            out.write(s.toString().getBytes(StandardCharsets.UTF_8));
        } catch (Exception e) {
            Log.w(TAG, "device state: " + e);
            return;
        }
        if (!tmp.renameTo(file)) Log.w(TAG, "device state: could not replace " + file);
    }
}
