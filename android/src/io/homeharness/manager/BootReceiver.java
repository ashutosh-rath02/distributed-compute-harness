package io.homeharness.manager;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** Starts the manager when the phone boots (unless turned off in the menu). */
public class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        if (!Intent.ACTION_BOOT_COMPLETED.equals(intent.getAction())) return;
        if (Harness.prefs(context).getBoolean(Harness.KEY_START_ON_BOOT, true)) {
            ManagerService.start(context);
        }
    }
}
