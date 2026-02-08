import org.gradle.api.file.DuplicatesStrategy
import org.gradle.jvm.tasks.Jar

val boxliteVersion = "0.5.9"
val boxliteJarName = "boxlite-java-highlevel-allplatforms-$boxliteVersion.jar"
val boxliteJar = rootProject.file("libs/$boxliteJarName")

val sanitizedBoxliteJar by tasks.registering(Jar::class) {
    group = "build"
    description = "Repackages boxlite jar and strips embedded Jackson classes to avoid runtime conflicts"

    archiveBaseName.set("boxlite-java-highlevel-allplatforms")
    archiveVersion.set(boxliteVersion)
    archiveClassifier.set("sanitized")
    destinationDirectory.set(layout.buildDirectory.dir("sanitized-libs"))

    duplicatesStrategy = DuplicatesStrategy.EXCLUDE

    from(zipTree(boxliteJar))

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
