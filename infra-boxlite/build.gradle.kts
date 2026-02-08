import org.gradle.api.file.DuplicatesStrategy
import org.gradle.jvm.tasks.Jar

val boxliteArtifact = "boxlite-java-highlevel-allplatforms"
val boxliteVersion = providers.gradleProperty("boxliteVersion")
    .orElse("0.5.10-coooolfan.1")
    .get()
val boxliteGithubOwner = providers.gradleProperty("boxliteGithubOwner")
    .orElse("Coooolfan")
    .get()
val boxliteGithubRepo = providers.gradleProperty("boxliteGithubRepo")
    .orElse("boxlite")
    .get()

val boxliteBinary = configurations.create("boxliteBinary") {
    isCanBeConsumed = false
    isCanBeResolved = true
    isVisible = false
    description = "Resolves boxlite highlevel jar from local libs or GitHub releases"
}

repositories {
    flatDir {
        dirs(rootProject.file("libs"))
    }
    ivy {
        name = "boxliteGithubRelease"
        url = uri("https://github.com/$boxliteGithubOwner/$boxliteGithubRepo/releases/download")
        patternLayout {
            artifact("v[revision]/[artifact]-[revision].[ext]")
        }
        metadataSources {
            artifact()
        }
        content {
            includeModule("com.coooolfan.boxlite", boxliteArtifact)
        }
    }
}

dependencies {
    add(boxliteBinary.name, "com.coooolfan.boxlite:$boxliteArtifact:$boxliteVersion@jar")
}

val sanitizedBoxliteJar by tasks.registering(Jar::class) {
    group = "build"
    description = "Repackages boxlite jar and strips embedded Jackson classes to avoid runtime conflicts"

    archiveBaseName.set(boxliteArtifact)
    archiveVersion.set(boxliteVersion)
    archiveClassifier.set("sanitized")
    destinationDirectory.set(layout.buildDirectory.dir("sanitized-libs"))

    duplicatesStrategy = DuplicatesStrategy.EXCLUDE

    // Resolve the dependency jar lazily from either local libs/ or GitHub Release.
    from({ boxliteBinary.resolve().map(::zipTree) })

    // Keep one Jackson stack on the app classpath. Spring Boot manages those versions.
    exclude("com/fasterxml/jackson/**")
    exclude("META-INF/maven/com.fasterxml.jackson.core/**")
    exclude("META-INF/versions/*/com/fasterxml/jackson/**")
    exclude("META-INF/*.SF")
    exclude("META-INF/*.DSA")
    exclude("META-INF/*.RSA")
}

dependencies {
    implementation(project(":core"))
    implementation(files(sanitizedBoxliteJar.flatMap { it.archiveFile }))
}
