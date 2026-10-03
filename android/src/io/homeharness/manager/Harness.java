package io.homeharness.manager;

import android.content.Context;
import android.content.SharedPreferences;
import android.content.pm.PackageInfo;
import android.content.res.AssetManager;
import android.net.ConnectivityManager;
import android.net.LinkAddress;
import android.net.LinkProperties;
import android.net.Network;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.Inet4Address;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.List;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

/**
 * Files, settings and the manager command line, shared by the service and
 * the activity. The state directory has the same layout as the Termux
 * manager's (~/.home-harness/state), so a state export from there imports
 * here unchanged.
 */
final class Harness {
    static final String PREFS = "harness";
    static final String KEY_KEEP_AWAKE = "keepAwake";
    static final String KEY_START_ON_BOOT = "startOnBoot";
    static final String KEY_ASKED_BATTERY = "askedBattery";
    static final String KEY_AGENTS_VERSION = "agentsVersion";
    static final int AGENT_PORT = 7420;
    static final int API_PORT = 7421;

    private Harness() {}

    static SharedPreferences prefs(Context c) {
        return c.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
    }

    static File stateDir(Context c) {
        return new File(c.getFilesDir(), "state");
    }

    static File agentsDir(Context c) {
        return new File(c.getFilesDir(), "agents");
    }

    static File logFile(Context c) {
        return new File(stateDir(c), "manager.log");
    }

    /** The manager binary: shipped as a "native library" because Android
     *  only lets an app execute files from its nativeLibraryDir. */
    static File binary(Context c) {
        return new File(c.getApplicationInfo().nativeLibraryDir, "libhomeharness_manager.so");
    }

    static String readFile(File f) {
        try (InputStream in = new FileInputStream(f)) {
            byte[] buf = new byte[(int) Math.min(f.length(), 1 << 20)];
            int n = 0, r;
            while (n < buf.length && (r = in.read(buf, n, buf.length - n)) > 0) n += r;
            return new String(buf, 0, n, StandardCharsets.UTF_8).trim();
        } catch (IOException e) {
            return "";
        }
    }

    static void writePrivate(File f, String content) throws IOException {
        f.getParentFile().mkdirs();
        try (OutputStream out = new FileOutputStream(f)) {
            out.write(content.getBytes(StandardCharsets.UTF_8));
        }
        f.setReadable(false, false);
        f.setReadable(true, true);
        f.setWritable(false, false);
        f.setWritable(true, true);
    }

    /** The token agents present to register, created once. */
    static String pairingToken(Context c) throws IOException {
        File f = new File(stateDir(c), "pairing-token");
        String t = readFile(f);
        if (!t.isEmpty()) return t;
        byte[] b = new byte[32];
        new SecureRandom().nextBytes(b);
        String token = hex(b);
        writePrivate(f, token);
        return token;
    }

    /** The operator token the manager creates on first start ("" before that). */
    static String operatorToken(Context c) {
        return readFile(new File(stateDir(c), "operator-token"));
    }

    static long versionCode(Context c) {
        try {
            PackageInfo pi = c.getPackageManager().getPackageInfo(c.getPackageName(), 0);
            return pi.getLongVersionCode();
        } catch (Exception e) {
            return 0;
        }
    }

    /** Agent builds the manager hands out for onboarding and self-update,
     *  copied from the APK's assets once per app version. */
    static void copyAgents(Context c) throws IOException {
        long version = versionCode(c);
        File dir = agentsDir(c);
        if (prefs(c).getLong(KEY_AGENTS_VERSION, -1) == version && dir.isDirectory()) return;
        File[] old = dir.listFiles();
        if (old != null) for (File f : old) f.delete();
        dir.mkdirs();
        AssetManager am = c.getAssets();
        String[] names = am.list("agents");
        if (names != null) {
            for (String n : names) {
                try (InputStream in = am.open("agents/" + n); OutputStream out = new FileOutputStream(new File(dir, n))) {
                    byte[] buf = new byte[1 << 16];
                    int r;
                    while ((r = in.read(buf)) > 0) out.write(buf, 0, r);
                }
            }
        }
        prefs(c).edit().putLong(KEY_AGENTS_VERSION, version).apply();
    }

    /** This phone's Wi-Fi/LAN IPv4 address, for agents to dial (null if
     *  offline). Java can read it where the manager process can't: apps
     *  targeting Android 11+ may not enumerate interfaces. */
    static String lanAddress(Context c) {
        ConnectivityManager cm = (ConnectivityManager) c.getSystemService(Context.CONNECTIVITY_SERVICE);
        if (cm == null) return null;
        Network n = cm.getActiveNetwork();
        if (n == null) return null;
        LinkProperties lp = cm.getLinkProperties(n);
        if (lp == null) return null;
        for (LinkAddress la : lp.getLinkAddresses()) {
            if (la.getAddress() instanceof Inet4Address && !la.getAddress().isLoopbackAddress()) {
                return la.getAddress().getHostAddress();
            }
        }
        return null;
    }

    /** The manager's command line: the same flags as the Termux launcher. */
    static List<String> command(Context c) throws IOException {
        File state = stateDir(c);
        state.mkdirs();
        List<String> cmd = new ArrayList<>();
        cmd.add(binary(c).getAbsolutePath());
        cmd.add("-addr");
        cmd.add(":" + AGENT_PORT);
        cmd.add("-api-addr");
        cmd.add("127.0.0.1:" + API_PORT);
        cmd.add("-pairing-token");
        cmd.add(pairingToken(c));
        cmd.add("-operator-token-file");
        cmd.add(new File(state, "operator-token").getAbsolutePath());
        cmd.add("-db");
        cmd.add(new File(state, "manager.db").getAbsolutePath());
        cmd.add("-tls-dir");
        cmd.add(new File(state, "tls").getAbsolutePath());
        cmd.add("-artifact-dir");
        cmd.add(new File(state, "artifacts").getAbsolutePath());
        String ip = lanAddress(c);
        if (ip != null) {
            cmd.add("-advertise-addr");
            cmd.add(ip + ":" + AGENT_PORT);
        }
        File[] agents = agentsDir(c).listFiles();
        if (agents != null) {
            for (File a : agents) {
                cmd.add("-agent-binary");
                cmd.add(a.getAbsolutePath());
            }
        }
        return cmd;
    }

    /** Runs the bundled manager binary for a one-shot command (state
     *  export/import) and returns its output; throws if it fails. */
    static String runOnce(Context c, String... args) throws IOException, InterruptedException {
        List<String> cmd = new ArrayList<>();
        cmd.add(binary(c).getAbsolutePath());
        for (String a : args) cmd.add(a);
        Process p = new ProcessBuilder(cmd).redirectErrorStream(true).start();
        StringBuilder out = new StringBuilder();
        try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
            String line;
            while ((line = r.readLine()) != null) out.append(line).append('\n');
        }
        int code = p.waitFor();
        if (code != 0) throw new IOException(out.toString().trim());
        return out.toString().trim();
    }

    /** Kills manager processes left over from an earlier run (Android can
     *  keep an app's child alive past the app, holding port 7420). Only
     *  our own binary, found by its command line. */
    static void killStale(Context c) {
        for (int pid : managerPids(c)) android.os.Process.killProcess(pid);
    }

    /** Whether any manager process (our binary) is still alive. */
    static boolean managerAlive(Context c) {
        return !managerPids(c).isEmpty();
    }

    private static List<Integer> managerPids(Context c) {
        List<Integer> pids = new ArrayList<>();
        String bin = binary(c).getAbsolutePath();
        File[] procs = new File("/proc").listFiles();
        if (procs == null) return pids;
        int self = android.os.Process.myPid();
        for (File p : procs) {
            String name = p.getName();
            if (!name.matches("\\d+")) continue;
            int pid = Integer.parseInt(name);
            if (pid == self) continue;
            // A zombie's cmdline is empty, so an exited manager never counts.
            if (readFile(new File(p, "cmdline")).startsWith(bin)) pids.add(pid);
        }
        return pids;
    }

    /** What answers on 127.0.0.1:7421. */
    enum Server { DOWN, OURS, IMPOSTOR }

    /** Checks that 127.0.0.1:7421 is this app's manager before anything
     *  sends it the operator token: while the manager is down any app on
     *  the phone can listen there, and the WebView would hand that app's
     *  page the login link (and the dashboard session in localStorage).
     *  Only the manager holding the token can answer the challenge
     *  (GET /server-proof, operatorauth.go). Not on the UI thread. */
    static Server checkServer(Context c) {
        String token = operatorToken(c);
        byte[] n = new byte[16];
        new SecureRandom().nextBytes(n);
        String nonce = hex(n);
        HttpURLConnection conn = null;
        try {
            conn = (HttpURLConnection) new URL("http://127.0.0.1:" + API_PORT + "/server-proof?nonce=" + nonce).openConnection();
            conn.setConnectTimeout(1000);
            conn.setReadTimeout(2000);
            conn.setUseCaches(false);
            if (conn.getResponseCode() != 200 || token.isEmpty()) return Server.IMPOSTOR;
            String body;
            try (InputStream in = conn.getInputStream()) {
                byte[] buf = new byte[4096];
                int len = 0, r;
                while (len < buf.length && (r = in.read(buf, len, buf.length - len)) > 0) len += r;
                body = new String(buf, 0, len, StandardCharsets.UTF_8);
            }
            String got = new JSONObject(body).optString("proof", "");
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(token.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            String want = hex(mac.doFinal(("harness-server-proof-v1:" + nonce).getBytes(StandardCharsets.UTF_8)));
            return MessageDigest.isEqual(got.getBytes(StandardCharsets.UTF_8), want.getBytes(StandardCharsets.UTF_8)) ? Server.OURS : Server.IMPOSTOR;
        } catch (IOException e) {
            return Server.DOWN; // nothing listening (yet)
        } catch (Exception e) {
            return Server.IMPOSTOR; // answered, but not with a valid proof
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder();
        for (byte x : b) sb.append(String.format("%02x", x));
        return sb.toString();
    }

    /** The last lines of the manager's log. */
    static String logTail(Context c, int maxChars) {
        String all = readFile(logFile(c));
        return all.length() <= maxChars ? all : all.substring(all.length() - maxChars);
    }
}
