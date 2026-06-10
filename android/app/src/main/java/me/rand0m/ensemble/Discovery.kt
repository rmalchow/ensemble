package me.rand0m.ensemble

import android.content.Context
import android.net.ConnectivityManager
import android.net.wifi.WifiManager
import android.util.Log
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.net.Inet4Address
import java.net.InetAddress
import javax.jmdns.JmDNS
import javax.jmdns.ServiceEvent
import javax.jmdns.ServiceInfo
import javax.jmdns.ServiceListener

/** One discovered ensemble master, built from its mDNS TXT record. */
data class Master(
    val id: String,
    val name: String,
    val host: String,
    val port: Int,
) {
    /** Picker label: the advertised name, else a short id. */
    val label: String get() = name.ifBlank { if (id.length >= 8) id.take(8) else id.ifBlank { host } }

    /** The master's web UI — controls the whole cluster via its proxy. */
    val url: String get() = "http://$host:$port/"
}

/**
 * Browses the LAN for ensemble masters (`_ensemble._tcp`) and exposes a live list.
 *
 * Uses jmdns rather than Android NsdManager: NsdManager's TXT/resolve is unreliable
 * and historically one-resolve-at-a-time, while jmdns gives stable TXT access and a
 * continuous browse across Android versions. A Wi-Fi multicast lock is held for the
 * lifetime of the browse so multicast (mDNS) actually reaches the app.
 *
 * start()/close() do network I/O — call them off the main thread.
 */
class EnsembleDiscovery(context: Context) {
    private val appContext = context.applicationContext

    private val _masters = MutableStateFlow<List<Master>>(emptyList())
    val masters: StateFlow<List<Master>> = _masters.asStateFlow()

    private var jmdns: JmDNS? = null
    private var lock: WifiManager.MulticastLock? = null
    private val found = LinkedHashMap<String, Master>() // keyed by mDNS service name

    private val listener = object : ServiceListener {
        override fun serviceAdded(event: ServiceEvent) {
            // Details (TXT + address) arrive asynchronously in serviceResolved.
            jmdns?.requestServiceInfo(event.type, event.name, 1500)
        }

        override fun serviceRemoved(event: ServiceEvent) {
            synchronized(found) {
                if (found.remove(event.name) != null) publish()
            }
        }

        override fun serviceResolved(event: ServiceEvent) {
            val master = event.info?.let(::toMaster) ?: return
            synchronized(found) {
                found[event.name] = master
                publish()
            }
        }
    }

    fun start() {
        val wifi = appContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
        lock = wifi.createMulticastLock("ensemble-mdns").apply {
            setReferenceCounted(true)
            acquire()
        }
        jmdns = JmDNS.create(localAddress()).also {
            it.addServiceListener(SERVICE_TYPE, listener)
            Log.i(TAG, "browsing $SERVICE_TYPE")
        }
    }

    fun close() {
        runCatching { jmdns?.removeServiceListener(SERVICE_TYPE, listener) }
        runCatching { jmdns?.close() }
        jmdns = null
        runCatching { lock?.release() }
        lock = null
    }

    private fun publish() {
        _masters.value = found.values.sortedBy { it.label.lowercase() }
    }

    /** Keep only role=master adverts that carry a usable http port + IPv4. */
    private fun toMaster(info: ServiceInfo): Master? {
        if (info.getPropertyString("role") != "master") return null
        val port = info.getPropertyString("http")?.toIntOrNull() ?: return null
        val host = info.inet4Addresses.firstOrNull()?.hostAddress ?: return null
        return Master(
            id = info.getPropertyString("id").orEmpty(),
            name = info.getPropertyString("name").orEmpty(),
            host = host,
            port = port,
        )
    }

    /** The device's own Wi-Fi IPv4 for jmdns to bind (avoids picking loopback). */
    private fun localAddress(): InetAddress {
        val cm = appContext.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        cm.activeNetwork?.let { net ->
            cm.getLinkProperties(net)?.linkAddresses?.forEach { la ->
                val a = la.address
                if (a is Inet4Address && !a.isLoopbackAddress) return a
            }
        }
        return runCatching { InetAddress.getLocalHost() }.getOrElse { InetAddress.getByName("0.0.0.0") }
    }

    companion object {
        private const val TAG = "EnsembleDiscovery"
        const val SERVICE_TYPE = "_ensemble._tcp.local."
    }
}
