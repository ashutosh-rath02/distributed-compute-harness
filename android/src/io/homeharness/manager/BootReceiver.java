package io.homeharness.manager;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** Starts the manager (or worker) when the device boots, and again after
 *  the app was updated (which stops it), unless turned off in the menu. */
public class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent.getAction();
        if (!Intent.ACTION_BOOT_COMPLETED.equals(action) && !Intent.ACTION_MY_PACKAGE_REPLACED.equals(action)) return;
        // Nothing to start until the first launch chose manager or worker.
        if (!Harness.mode(context).isEmpty() && Harness.prefs(context).getBoolean(Harness.KEY_START_ON_BOOT, true)) {
            ManagerService.start(context);
        }
    }
}
