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
    }
}

val libboxArtifact = file("libs/libbox.aar")
if (!libboxArtifact.isFile) {
    throw GradleException(
        "Missing ${libboxArtifact.absolutePath}. This plugin requires the real sing-box 1.14.4 " +
            "libbox AAR built with the pinned Tailscale Go toolchain. From the sing-box-flutter " +
            "root run: python3 tool/build_libbox.py --target android " +
            "(JDK 17, Android SDK and NDK 28.0.13004108 required), or install a published artifact " +
            "with: python3 tool/install_libbox.py --target android --release v0.1.0. " +
            "See native-core.json and the project README. No fallback backend is provided."
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

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    sourceSets {
        getByName("main") {
            java.srcDirs("src/main/kotlin")
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
