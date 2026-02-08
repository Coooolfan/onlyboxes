import org.gradle.jvm.tasks.Jar
import org.springframework.boot.gradle.tasks.bundling.BootJar

plugins {
    id("org.springframework.boot") version "4.0.2"
}

dependencies {
    implementation(project(":core"))
    implementation(project(":infra-boxlite"))

    implementation(platform("org.springframework.boot:spring-boot-dependencies:4.0.2"))
    implementation("org.springframework.boot:spring-boot-starter-web:4.0.2")
    implementation(platform("org.springframework.ai:spring-ai-bom:2.0.0-M2"))
    implementation("org.springframework.ai:spring-ai-starter-mcp-server-webmvc")
}

tasks.named<BootJar>("bootJar") {
    archiveClassifier.set("all")
    mainClass.set("com.coooolfan.onlyboxes.app.App")
}

tasks.named<Jar>("jar") {
    enabled = false
}
