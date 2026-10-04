package io.homeharness.manager;

import android.app.Activity;
import android.app.AlertDialog;
import android.graphics.Typeface;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.text.InputType;
import android.view.Menu;
import android.view.MenuItem;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * This device as a worker: the bundled agent runs in the background
 * service, joins a manager by approval (agent -pair), and runs the tasks it
 * sends. The screen shows where that stands — looking for the manager,
 * waiting for approval (with the pairing code to compare), or connected —
 * read from the agent's own log.
 */
public class WorkerActivity extends Activity {
    private static final int M_ADDR = 1, M_LOG = 2, M_AWAKE = 3, M_BOOT = 4, M_BATTERY = 5, M_SWITCH = 6, M_FORGET = 7;
    private static final Pattern CODE = Pattern.compile("pairing: approve this device on the manager, code (\\d{3} \\d{3})");
    private static final Pattern CONNECTED = Pattern.compile("registered with manager at (\\S+)");

    private final Handler ui = new Handler(Looper.getMainLooper());
    private TextView status, code, detail, update;
    private Button toggle, install;
    private final Runnable refresher = new Runnable() {
        @Override
        public void run() {
            refresh();
            ui.postDelayed(this, 1000);
        }
    };

    @Override
    protected void onCreate(Bundle saved) {
        super.onCreate(saved);
        int pad = (int) (24 * getResources().getDisplayMetrics().density);
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(pad, pad, pad, pad);

        TextView title = new TextView(this);
        title.setText("This device is a worker");
        title.setTextSize(22);
        title.setTypeface(Typeface.DEFAULT_BOLD);
        box.addView(title);

        TextView name = new TextView(this);
        name.setText("Name: " + Harness.deviceName(this));
        name.setPadding(0, pad / 4, 0, pad);
        box.addView(name);

        status = new TextView(this);
        status.setTextSize(18);
        box.addView(status);

        code = new TextView(this);
        code.setTextSize(44);
        code.setTypeface(Typeface.MONOSPACE, Typeface.BOLD);
        code.setPadding(0, pad / 2, 0, pad / 2);
        box.addView(code);

        detail = new TextView(this);
        detail.setTextSize(14);
        detail.setPadding(0, 0, 0, pad);
        box.addView(detail);

        // An app update from the manager waiting for the owner's OK.
        update = new TextView(this);
        update.setTextSize(14);
        update.setTypeface(Typeface.DEFAULT_BOLD);
        box.addView(update);
        install = new Button(this);
        install.setText("Install the update");
        install.setOnClickListener(v -> {
            android.content.Intent screen = AppUpdater.confirm;
            if (screen != null) startActivity(screen);
        });
        box.addView(install);

        Button addr = new Button(this);
        addr.setText("Manager's address…");
        addr.setOnClickListener(v -> askAddress());
        box.addView(addr);

        toggle = new Button(this);
        toggle.setOnClickListener(v -> {
            if (ManagerService.running) ManagerService.stop(this);
            else ManagerService.start(this);
            ui.postDelayed(this::refresh, 300);
        });
        box.addView(toggle);

        ScrollView scroll = new ScrollView(this);
        scroll.addView(box);
        setContentView(scroll);

        if (Build.VERSION.SDK_INT >= 33) requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 9);
        ManagerService.start(this);
        askBatteryOnce();
    }

    @Override
    protected void onResume() {
        super.onResume();
        ui.post(refresher);
    }

    @Override
    protected void onPause() {
        super.onPause();
        ui.removeCallbacks(refresher);
    }

    /** Shows the state the agent's log ends in (newest relevant line wins). */
    private void refresh() {
        toggle.setText(ManagerService.running ? "Stop the worker" : "Start the worker");
        String u = AppUpdater.status;
        update.setText(u);
        update.setVisibility(u.isEmpty() ? View.GONE : View.VISIBLE);
        install.setVisibility(AppUpdater.confirm != null ? View.VISIBLE : View.GONE);
        code.setVisibility(View.GONE);
        if (!ManagerService.running) {
            status.setText("Stopped");
            detail.setText("This device doesn't run tasks until you start it again.");
            return;
        }
        String[] lines = Harness.readTail(Harness.workerLog(this), 16000).split("\n");
        for (int i = lines.length - 1; i >= 0; i--) {
            String l = lines[i];
            Matcher m;
            if ((m = CONNECTED.matcher(l)).find()) {
                status.setText("Connected to the manager");
                detail.setText("At " + m.group(1) + ". It runs the tasks the manager sends. Keep this app installed; it starts by itself.");
                return;
            }
            if ((m = CODE.matcher(l)).find()) {
                status.setText("Approve this device on the manager's phone");
                code.setText(m.group(1));
                code.setVisibility(View.VISIBLE);
                detail.setText("Check that the manager shows this same code before you approve it there.");
                return;
            }
            if (l.contains("WARNING: the manager shows pairing code")) {
                status.setText("The codes don't match");
                detail.setText("Something is between this device and the manager. Don't approve it. Check that both are on your own Wi-Fi, then restart the worker.");
                return;
            }
            if (l.contains("isn't accepting new devices")) {
                status.setText("The manager isn't accepting new devices");
                detail.setText("On the manager, open \"Add a device\" (it stays open 15 minutes). This device asks again by itself.");
                return;
            }
            if (l.contains("node revoked by operator")) {
                status.setText("The manager removed this device");
                detail.setText("It stays out until whoever runs the manager lets it back in; then it asks to join again by itself.");
                return;
            }
            if (l.contains("declined this device")) {
                status.setText("The manager declined this device");
                detail.setText("It asks again in a few minutes.");
                return;
            }
            if (l.contains("no manager beacon") || l.contains("no manager found")) {
                status.setText("Can't find the manager on this Wi-Fi");
                detail.setText("Make sure the manager's phone is on and on the same Wi-Fi, or enter its address (tap \"Manager's address\").");
                return;
            }
            if (l.contains("connection error")) {
                status.setText("Can't reach the manager. Retrying…");
                detail.setText(l.substring(l.indexOf("connection error")));
                return;
            }
        }
        status.setText("Looking for the manager on this Wi-Fi…");
        detail.setText("");
    }

    private void askAddress() {
        EditText input = new EditText(this);
        input.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_URI);
        input.setHint("192.168.1.20");
        input.setText(Harness.prefs(this).getString(Harness.KEY_MANAGER_ADDR, ""));
        new AlertDialog.Builder(this)
                .setTitle("Manager's address")
                .setMessage("Usually not needed: the worker finds the manager on your Wi-Fi by itself. Enter the manager phone's IP address if it can't (shown in the manager's notification).")
                .setView(input)
                .setPositiveButton("Save", (d, w) -> saveAddress(input.getText().toString().trim()))
                .setNeutralButton("Find it automatically", (d, w) -> saveAddress(""))
                .setNegativeButton("Cancel", null)
                .show();
    }

    private void saveAddress(String addr) {
        if (!addr.isEmpty()) {
            if (!addr.matches("[A-Za-z0-9.-]{1,253}(:[0-9]{1,5})?")) {
                new AlertDialog.Builder(this).setMessage("That isn't an address like 192.168.1.20.").setPositiveButton("OK", null).show();
                return;
            }
            if (!addr.contains(":")) addr = addr + ":" + Harness.AGENT_PORT;
        }
        Harness.prefs(this).edit().putString(Harness.KEY_MANAGER_ADDR, addr).apply();
        restartWorker();
    }

    /** Drops the pinned manager certificate and any typed address; the
     *  device keeps its identity and pairs again (approval needed). */
    private void forgetManager() {
        ManagerService.stop(this);
        new java.io.File(new java.io.File(Harness.workerDir(this), "identity"), "manager-fingerprint").delete();
        Harness.prefs(this).edit().putString(Harness.KEY_MANAGER_ADDR, "").apply();
        ui.postDelayed(() -> ManagerService.start(this), 800);
    }

    private void restartWorker() {
        ManagerService.stop(this);
        ui.postDelayed(() -> ManagerService.start(this), 800);
    }

    private void askBatteryOnce() {
        if (Harness.prefs(this).getBoolean(Harness.KEY_ASKED_BATTERY, false)) return;
        Harness.prefs(this).edit().putBoolean(Harness.KEY_ASKED_BATTERY, true).apply();
        PowerManager pm = getSystemService(PowerManager.class);
        if (pm.isIgnoringBatteryOptimizations(getPackageName())) return;
        new AlertDialog.Builder(this)
                .setTitle("Keep the worker running")
                .setMessage("Allow Home Harness to run in the background without battery restrictions, or Android may stop it.")
                .setPositiveButton("Allow", (d, w) -> Harness.openBatterySettings(this))
                .setNegativeButton("Later", null)
                .show();
    }

    @Override
    public boolean onCreateOptionsMenu(Menu menu) {
        menu.add(0, M_ADDR, 0, "Manager's address…");
        menu.add(0, M_LOG, 0, "Worker log");
        menu.add(0, M_AWAKE, 0, "Keep Wi-Fi and CPU awake").setCheckable(true)
                .setChecked(Harness.prefs(this).getBoolean(Harness.KEY_KEEP_AWAKE, true));
        menu.add(0, M_BOOT, 0, "Start when the device boots").setCheckable(true)
                .setChecked(Harness.prefs(this).getBoolean(Harness.KEY_START_ON_BOOT, true));
        menu.add(0, M_BATTERY, 0, "Battery optimization…");
        menu.add(0, M_FORGET, 0, "Forget this manager…");
        menu.add(0, M_SWITCH, 0, "Run the manager here instead…");
        return true;
    }

    @Override
    public boolean onOptionsItemSelected(MenuItem item) {
        switch (item.getItemId()) {
            case M_ADDR:
                askAddress();
                return true;
            case M_LOG:
                new AlertDialog.Builder(this).setTitle("Worker log")
                        .setMessage(Harness.readTail(Harness.workerLog(this), 6000))
                        .setPositiveButton("OK", null).show();
                return true;
            case M_AWAKE:
            case M_BOOT: {
                String key = item.getItemId() == M_AWAKE ? Harness.KEY_KEEP_AWAKE : Harness.KEY_START_ON_BOOT;
                boolean on = !item.isChecked();
                item.setChecked(on);
                Harness.prefs(this).edit().putBoolean(key, on).apply();
                if (item.getItemId() == M_AWAKE) restartWorker();
                return true;
            }
            case M_BATTERY:
                Harness.openBatterySettings(this);
                return true;
            case M_FORGET:
                new AlertDialog.Builder(this)
                        .setTitle("Forget this manager?")
                        .setMessage("This device stops trusting its manager and pairs again with whichever manager answers first on this Wi-Fi: you approve it there with a new code. "
                                + "If the old manager is still around, this device may simply reconnect to it; to cut it off, revoke this device on that manager.")
                        .setPositiveButton("Forget", (d, w) -> forgetManager())
                        .setNegativeButton("Cancel", null)
                        .show();
                return true;
            case M_SWITCH:
                new AlertDialog.Builder(this)
                        .setTitle("Run the manager here?")
                        .setMessage("This device stops working for its manager and runs its own manager instead.")
                        .setPositiveButton("Switch", (d, w) -> Harness.switchMode(this, Harness.MODE_MANAGER))
                        .setNegativeButton("Cancel", null)
                        .show();
                return true;
        }
        return super.onOptionsItemSelected(item);
    }
}
