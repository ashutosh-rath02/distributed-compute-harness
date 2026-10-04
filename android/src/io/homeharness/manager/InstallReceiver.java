package io.homeharness.manager;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageInstaller;

/** The installer's answer to an app update (AppUpdater). */
public class InstallReceiver extends BroadcastReceiver {
    @Override
    @SuppressWarnings("deprecation")
    public void onReceive(Context c, Intent i) {
        int st = i.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE);
        switch (st) {
            case PackageInstaller.STATUS_PENDING_USER_ACTION: {
                Intent screen = i.getParcelableExtra(Intent.EXTRA_INTENT);
                if (screen != null) AppUpdater.askToConfirm(c, screen);
                break;
            }
            case PackageInstaller.STATUS_SUCCESS:
                AppUpdater.installed(c);
                break;
            case PackageInstaller.STATUS_FAILURE_ABORTED:
                if (!AppUpdater.retryAfterAbort(c)) {
                    AppUpdater.fail(c, "The update wasn't installed (canceled). Run the update from the manager again to retry.");
                }
                break;
            default:
                AppUpdater.fail(c, "Couldn't install the update: " + i.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE));
        }
    }
}
