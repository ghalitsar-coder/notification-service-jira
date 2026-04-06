pipeline {
    agent any

    options {
        timestamps()
        buildDiscarder(logRotator(numToKeepStr: '10'))
    }

    triggers {
        pollSCM('* * * * *')
    }

    environment {
        IMAGE_NAME    = 'notification-service'
        K8S_DEPLOY    = 'notification-deployment'
        K8S_CONTAINER = 'notification'
        K8S_NAMESPACE = 'cloud-dev'
    }

    stages {

        stage('📥 Checkout') {
            steps {
                checkout scm
            }
        }

        stage('🏷️ Get Version from Git Tag') {
            steps {
                script {
                    // Mengambil tag terbaru + jumlah commit di depannya + hash commit
                    // Format: v1.0.0-3-g1a2b3c (artinya 3 commit setelah v1.0.0)
                    // Jika tidak ada tag sama sekali, akan me-return hash commit
                    env.IMAGE_TAG = sh(script: "git describe --tags --always || echo latest", returnStdout: true).trim()
                    echo "Deploying version tag: ${env.IMAGE_TAG}"
                }
            }
        }

        stage('🔎 Get Minikube IP') {
            steps {
                script {
                    // Ambil IP Minikube secara dinamis dari node K8s
                    env.MINIKUBE_IP = sh(
                        script: "kubectl get nodes -o jsonpath='{.items[0].status.addresses[0].address}'",
                        returnStdout: true
                    ).trim()
                    echo "Minikube IP: ${env.MINIKUBE_IP}"
                }
            }
        }

        stage('🐳 Build Docker Image') {
            steps {
                sh """
                    echo "Building ${IMAGE_NAME}:${IMAGE_TAG}..."
                    docker build -t ${IMAGE_NAME}:${IMAGE_TAG} .
                    docker tag ${IMAGE_NAME}:${IMAGE_TAG} ${IMAGE_NAME}:latest
                    echo "✅ Image built: ${IMAGE_NAME}:${IMAGE_TAG}"
                """
            }
        }

        stage('🔍 Verify Image') {
            steps {
                sh "docker images ${IMAGE_NAME}"
            }
        }

        stage('🚀 Deploy to Minikube') {
            steps {
                sh """
                    echo "Updating deployment image..."
                    kubectl set image deployment/${K8S_DEPLOY} \
                        ${K8S_CONTAINER}=${IMAGE_NAME}:${IMAGE_TAG} \
                        -n ${K8S_NAMESPACE}

                    echo "Waiting for rollout to complete..."
                    kubectl rollout status deployment/${K8S_DEPLOY} -n ${K8S_NAMESPACE} --timeout=120s

                    echo "✅ Deployment successful!"
                    kubectl get pods -l app=notification -n ${K8S_NAMESPACE}
                """
            }
        }

        stage('🏥 Health Check') {
            steps {
                sh """
                    # Port 30081 disesuaikan dengan asumsi NodePort untuk notification-service
                    echo "Notification Service URL: http://${MINIKUBE_IP}:30081/health"
                    sleep 10
                    curl -sf http://${MINIKUBE_IP}:30081/health && echo "✅ Notification Service healthy!" || echo "⚠️  Health check failed (mungkin masih starting)"
                """
            }
        }
    }

    post {
        success {
            echo '🎉 Notification Service CI/CD berhasil!'
        }
        failure {
            sh "kubectl describe deployment/${K8S_DEPLOY} -n ${K8S_NAMESPACE} || true"
            sh "kubectl logs -l app=notification -n ${K8S_NAMESPACE} --tail=50 || true"
            echo '❌ Pipeline gagal. Lihat logs di atas.'
        }
        always {
            sh 'docker image prune -f --filter "until=24h" || true'
        }
    }
}
