package me.rand0m.ensemble

import android.Manifest
import android.os.Build
import android.os.Bundle
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Button
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    private lateinit var discovery: EnsembleDiscovery

    // Android 13+: request NEARBY_WIFI_DEVICES, then browse regardless of the result
    // (older devices need no runtime grant; the multicast lock does the work).
    private val permLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { startDiscovery() }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        discovery = EnsembleDiscovery(this)
        setContent { MaterialTheme { App(discovery) } }

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            permLauncher.launch(Manifest.permission.NEARBY_WIFI_DEVICES)
        } else {
            startDiscovery()
        }
    }

    private fun startDiscovery() {
        lifecycleScope.launch(Dispatchers.IO) { runCatching { discovery.start() } }
    }

    override fun onDestroy() {
        super.onDestroy()
        // close() does network I/O; run it off the main thread.
        lifecycleScope.launch(Dispatchers.IO) { runCatching { discovery.close() } }
    }
}

@Composable
fun App(discovery: EnsembleDiscovery) {
    val masters by discovery.masters.collectAsState()
    var url by remember { mutableStateOf<String?>(null) }

    if (url == null) {
        PickerScreen(masters) { url = it }
    } else {
        WebScreen(url!!) { url = null }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PickerScreen(masters: List<Master>, onOpen: (String) -> Unit) {
    var manual by remember { mutableStateOf("") }
    Scaffold(topBar = { TopAppBar(title = { Text("ensemble") }) }) { pad ->
        Column(
            modifier = Modifier.padding(pad).padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Text("Masters on this network", style = MaterialTheme.typography.titleMedium)
            if (masters.isEmpty()) {
                Text("Searching…", style = MaterialTheme.typography.bodyMedium)
            }
            LazyColumn(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                items(masters, key = { it.id.ifBlank { it.url } }) { m ->
                    ElevatedCard(onClick = { onOpen(m.url) }, modifier = Modifier.fillMaxWidth()) {
                        Column(Modifier.padding(16.dp)) {
                            Text(m.label, style = MaterialTheme.typography.titleMedium)
                            Text("${m.host}:${m.port}", style = MaterialTheme.typography.bodySmall)
                        }
                    }
                }
            }
            HorizontalDivider()
            Text("Or connect by address", style = MaterialTheme.typography.titleSmall)
            Row(
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                OutlinedTextField(
                    value = manual,
                    onValueChange = { manual = it },
                    label = { Text("host or host:port") },
                    singleLine = true,
                    modifier = Modifier.weight(1f),
                )
                Button(
                    onClick = { if (manual.isNotBlank()) onOpen(normalizeUrl(manual)) },
                    enabled = manual.isNotBlank(),
                ) { Text("Open") }
            }
        }
    }
}

/** "192.168.1.5" → http://192.168.1.5:8080/ ; pass-through if a scheme/port is given. */
private fun normalizeUrl(input: String): String {
    var s = input.trim()
    if (!s.startsWith("http://") && !s.startsWith("https://")) {
        if (!s.contains(":")) s = "$s:8080"
        s = "http://$s"
    }
    return if (s.endsWith("/")) s else "$s/"
}

@Composable
fun WebScreen(url: String, onBack: () -> Unit) {
    var webView by remember { mutableStateOf<WebView?>(null) }
    BackHandler {
        val wv = webView
        if (wv != null && wv.canGoBack()) wv.goBack() else onBack()
    }
    AndroidView(
        modifier = Modifier.fillMaxSize(),
        factory = { ctx ->
            WebView(ctx).apply {
                settings.javaScriptEnabled = true
                settings.domStorageEnabled = true
                webViewClient = WebViewClient() // keep navigation inside the WebView
                webView = this
                loadUrl(url)
            }
        },
    )
}
