dependencies {
    implementation(project(":core"))
    implementation(project(":infra-boxlite"))

    implementation(platform("org.noear:solon-parent:3.9.1"))
    implementation("org.noear:solon-web")
    implementation("org.noear:solon-ai")
    implementation("org.noear:solon-ai-mcp")
    implementation("org.noear:solon-ai-flow")
    implementation("org.noear:solon-logging-logback-jakarta")

    testImplementation("org.noear:solon-test")
}

tasks.withType<Jar>().configureEach {
    manifest {
        attributes["Main-Class"] = "com.coooolfan.onlyboxes.app.App"
    }
}

val fatJar by tasks.registering(Jar::class) {
    group = "build"
    description = "Builds an executable fat jar for the app module"
    archiveClassifier.set("all")

    dependsOn(configurations.runtimeClasspath)

    duplicatesStrategy = DuplicatesStrategy.EXCLUDE
    from(configurations.runtimeClasspath.get().map {
        if (it.isDirectory) it else zipTree(it)
    })
    from(sourceSets.main.get().output)

    manifest {
        attributes["Main-Class"] = "com.coooolfan.onlyboxes.app.App"
    }
}

tasks.named("assemble") {
    dependsOn(fatJar)
}
