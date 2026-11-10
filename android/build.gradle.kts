import groovy.json.JsonSlurper
import java.io.File
import java.nio.file.Files
import java.security.MessageDigest
import org.yaml.snakeyaml.Yaml
import org.yaml.snakeyaml.LoaderOptions
import org.yaml.snakeyaml.constructor.SafeConstructor

group = "dev.idy.sing_box_flutter"
version = "1.0-SNAPSHOT"

buildscript {
    val kotlinVersion = "2.4.0"
    repositories {
        google()
        mavenCentral()
    }

    dependencies {
        classpath("com.android.tools.build:gradle:9.1.0")
        classpath("org.jetbrains.kotlin:kotlin-gradle-plugin:$kotlinVersion")
        classpath("org.yaml:snakeyaml:2.4")
    }
}

val explicitFlutterRoot = providers.gradleProperty("singBoxFlutterProject").orNull
    ?: System.getenv("SINGBOX_HOST_PROJECT")
val moduleFlutterRoot = rootProject.findProject(":flutter")?.projectDir?.parentFile?.parentFile
val flutterProject = when {
    !explicitFlutterRoot.isNullOrBlank() -> {
        val supplied = File(explicitFlutterRoot)
        if (!supplied.isAbsolute || !File(supplied, "pubspec.yaml").isFile) {
            throw GradleException("SINGBOX_HOST_PROJECT/singBoxFlutterProject must name an absolute Flutter project containing pubspec.yaml")
        }
        supplied.canonicalFile
    }
    moduleFlutterRoot != null -> {
        if (!File(moduleFlutterRoot, "pubspec.yaml").isFile) {
            throw GradleException("Cannot resolve the Flutter module. Set SINGBOX_HOST_PROJECT to its absolute project directory.")
        }
        moduleFlutterRoot.canonicalFile
    }
    else -> rootProject.projectDir.parentFile.canonicalFile
}
val hostPubspec = File(flutterProject, "pubspec.yaml")
val nativeReceiptFile = File(flutterProject, ".dart_tool/sing_box_flutter/native.json")
val buildCoreHint = "From your Flutter app run: dart run sing_box_flutter build --target android"
fun digest(file: File): String = MessageDigest.getInstance("SHA-256").run {
    file.inputStream().use { stream ->
        val buffer = ByteArray(1024 * 1024)
        var length = stream.read(buffer)
        while (length >= 0) {
            if (length > 0) update(buffer, 0, length)
            length = stream.read(buffer)
        }
    }
    digest().joinToString("") { "%02x".format(it.toInt() and 0xff) }
}
fun hostArtifact(relative: Any?): File {
    val path = relative as? String ?: throw GradleException("Invalid native artifact path. $buildCoreHint")
    val raw = File(flutterProject, path)
    val boundary = File(flutterProject, ".dart_tool/sing_box_flutter").toPath()
    if (File(path).isAbsolute || path.split('/', '\\').contains("..") || !raw.toPath().startsWith(boundary)) {
        throw GradleException("Native artifact escapes this Flutter project's cache. $buildCoreHint")
    }
    var ancestor: File? = raw
    while (ancestor != null && ancestor != flutterProject) {
        if (Files.isSymbolicLink(ancestor.toPath())) throw GradleException("Native artifact path contains a symlink. $buildCoreHint")
        ancestor = ancestor.parentFile
    }
    return raw
}
val configuredHost = if (hostPubspec.isFile) {
    try {
        val settings = Yaml(SafeConstructor(LoaderOptions())).load<Any>(hostPubspec.readText()) as? Map<*, *>
            ?: throw GradleException("Host pubspec must be a mapping")
        if (!settings.containsKey("sing_box_flutter")) false else {
            val config = settings["sing_box_flutter"] as? Map<*, *>
                ?: throw GradleException("sing_box_flutter must be a mapping")
            if (config.keys.any { it != "core" }) throw GradleException("Unknown sing_box_flutter setting")
            config.containsKey("core")
        }
    } catch (error: Exception) {
        throw GradleException("Invalid host core configuration. $buildCoreHint", error)
    }
} else false
val nativeReceipt = if (configuredHost && nativeReceiptFile.exists()) {
    try {
        if (Files.isSymbolicLink(nativeReceiptFile.toPath())) throw GradleException("Native receipt must not be a symlink")
        (JsonSlurper().parse(nativeReceiptFile) as? Map<*, *>)
            ?: throw GradleException("Invalid selectable core receipt")
    } catch (error: Exception) {
        throw GradleException("Cannot read selectable core receipt. $buildCoreHint", error)
    }
} else null
fun verifyCurrentRecipe() {
    val checker = file("../tool/core_recipe.py")
    val candidates = listOfNotNull(
        System.getenv("SINGBOX_PYTHON")?.takeIf { it.isNotBlank() },
        "python3.14", "python3.13", "python3.12", "python3.11", "python3", "python"
    ).distinct()
    for (python in candidates) {
        val process = try {
            ProcessBuilder(python, "-I", checker.absolutePath,
                "--project-dir", flutterProject.absolutePath, "--target", "android")
                .redirectErrorStream(true).start()
        } catch (_: java.io.IOException) { continue }
        process.inputStream.bufferedReader().use { it.readText() }
        if (process.waitFor() != 0) {
            throw GradleException("The selected core recipe is stale or cannot be verified. $buildCoreHint")
        }
        return
    }
    throw GradleException("Python 3 is required to verify the selected core recipe; set SINGBOX_PYTHON. $buildCoreHint")
}
val selectedAndroid = nativeReceipt?.let { receipt ->
    if (receipt["schema"] != 1 || receipt["version"] != "1.14.4" ||
        !hostPubspec.isFile || digest(hostPubspec) != receipt["pubspec_sha256"]) {
        throw GradleException("The native core does not match this app's pubspec. $buildCoreHint")
    }
    val entry = (receipt["artifacts"] as? Map<*, *>)?.get("android") as? Map<*, *>
        ?: throw GradleException("No Android artifact for the selected core. $buildCoreHint")
    verifyCurrentRecipe()
    val aar = hostArtifact(entry["path"])
    val capabilities = hostArtifact(entry["capabilities"])
    val expected = (entry["sha256"] as? Map<*, *>)?.get("libbox.aar")
    if (!aar.isFile || digest(aar) != expected || !capabilities.isFile ||
        digest(capabilities) != entry["capabilities_sha256"]) {
        throw GradleException("Selected core artifacts changed or are incomplete. $buildCoreHint")
    }
    val bundled = File(aar.parentFile, "assets/sing_box_flutter/core-capabilities.json")
    if (!bundled.isFile || digest(bundled) != entry["capabilities_sha256"]) {
        throw GradleException("Selected core capabilities asset is missing or changed. $buildCoreHint")
    }
    val manifest = JsonSlurper().parse(capabilities) as? Map<*, *>
    if (manifest?.get("fingerprint") != receipt["fingerprint"] || manifest?.get("features") != receipt["features"]) {
        throw GradleException("Selected core capabilities do not match the receipt. $buildCoreHint")
    }
    aar
}
if (configuredHost && nativeReceipt == null) {
    throw GradleException("This app selects native core features but has no built artifact. $buildCoreHint")
}
val libboxArtifact = selectedAndroid ?: file("libs/libbox.aar")
if (!libboxArtifact.isFile) {
    throw GradleException(
        "Missing native sing-box 1.14.4 AAR. $buildCoreHint. " +
            "Source builds require JDK 17, Android SDK and NDK 28.0.13004108. See doc/native-build.md."
    )
}

// A repository dependency lets Flutter host apps package the AAR's JNI libraries.
// Direct local AAR file dependencies cannot be bundled by an Android library project.
rootProject.allprojects {
    repositories {
        google()
        mavenCentral()
        flatDir { dirs(libboxArtifact.parentFile) }
    }
}

plugins {
    id("com.android.library")
}

android {
    namespace = "dev.idy.sing_box_flutter"

    compileSdk = 36
    ndkVersion = "28.2.13676358"

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    sourceSets {
        getByName("main") {
            java.srcDirs("src/main/kotlin")
            if (selectedAndroid != null) assets.srcDir(File(selectedAndroid.parentFile, "assets"))
        }
        getByName("test") {
            java.srcDirs("src/test/kotlin")
        }
    }

    defaultConfig {
        minSdk = 24
        consumerProguardFiles("consumer-rules.pro")
    }

    testOptions {
        unitTests {
            isIncludeAndroidResources = true
            all {
                it.outputs.upToDateWhen { false }

                it.testLogging {
                    events("passed", "skipped", "failed", "standardOut", "standardError")
                    showStandardStreams = true
                }
            }
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget = org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17
    }
}

dependencies {
    implementation(mapOf("name" to "libbox", "ext" to "aar"))
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.robolectric:robolectric:4.16.1")
}
