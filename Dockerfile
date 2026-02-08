FROM eclipse-temurin:25-jre-noble

WORKDIR /opt/onlyboxes

COPY app/build/libs/app-all.jar app.jar

EXPOSE 8084

USER 10001

ENTRYPOINT ["java", "-jar", "/opt/onlyboxes/app.jar"]
