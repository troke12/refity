import { useState, useEffect, useRef } from 'react';
import { Link } from 'react-router-dom';
import { dashboardAPI, ftpAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import { formatTB } from '../utils/formatTB';

import StatCard from '../components/StatCard';
import CreateGroupModal from '../components/CreateGroupModal';
import Navbar from '../components/Navbar';
import Footer from '../components/Footer';
import './Dashboard.css';

function Dashboard() {
  const [data, setData] = useState(null);
  const [ftpUsage, setFtpUsage] = useState(null);
  const [ftpUsageEnabled, setFtpUsageEnabled] = useState(true); // false when backend returns enabled: false (e.g. not using Hetzner)
  const ftpIntervalRef = useRef(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [showCreateGroupModal, setShowCreateGroupModal] = useState(false);

  const loadData = async () => {
    try {
      const dashboardData = await dashboardAPI.get();
      setData(dashboardData);
    } catch (error) {
      console.error('Failed to load dashboard:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  const loadFTPUsage = async () => {
    if (!ftpUsageEnabled) return;
    try {
      const usage = await ftpAPI.getUsage();
      if (usage && usage.enabled === false) {
        setFtpUsageEnabled(false);
        if (ftpIntervalRef.current) {
          clearInterval(ftpIntervalRef.current);
          ftpIntervalRef.current = null;
        }
        return;
      }
      if (usage && usage.error) {
        setFtpUsage(null);
      } else {
        setFtpUsage(usage);
      }
    } catch (error) {
      setFtpUsage(null);
    }
  };

  useEffect(() => {
    loadData();
    loadFTPUsage();
    ftpIntervalRef.current = setInterval(loadFTPUsage, 2 * 60 * 1000);
    return () => {
      if (ftpIntervalRef.current) clearInterval(ftpIntervalRef.current);
    };
  }, []);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };

  const handleGroupCreated = () => {
    setShowCreateGroupModal(false);
    loadData();
  };


  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  return (
    <div className="container-main">
      <Navbar onRefresh={handleRefresh} isRefreshing={isRefreshing} />

      <div className="container-fluid px-0">
        <div className="stats-section">
          <div className="row g-3">
            <div className={`col-sm-6 ${ftpUsageEnabled ? 'col-md-2' : 'col-md-4'}`}>
              <StatCard
                icon="bi-images"
                label="Total Images"
                value={data?.total_images || 0}
                gradient="primary"
              />
            </div>
            <div className={`col-sm-6 ${ftpUsageEnabled ? 'col-md-2' : 'col-md-4'}`}>
              <StatCard
                icon="bi-folder"
                label="Total Groups"
                value={data?.groups?.length || 0}
                gradient="success"
              />
            </div>
            <div className={`col-sm-6 ${ftpUsageEnabled ? 'col-md-3' : 'col-md-4'}`}>
              <StatCard
                icon="bi-hdd"
                label="Total Size"
                value={formatBytes(data?.total_size || 0)}
                gradient="warning"
              />
            </div>
            {ftpUsageEnabled && (
              <div className="col-md-5 col-sm-6">
                <StatCard
                  icon="bi-server"
                  label="FTP Usage"
                  value={ftpUsage ? `${formatBytes(ftpUsage.used_size)} / ${formatTB(ftpUsage.total_size_tb)}` : 'N/A'}
                  gradient="primary"
                />
              </div>
            )}
          </div>
        </div>

        <div className="card">
          <div className="card-header">
            <div className="card-header-title-wrapper">
              <div>
                <h5>
                  <i className="bi bi-folder"></i>
                  Groups
                </h5>
                <small>List of all groups and their repositories</small>
              </div>
              <button
                onClick={() => setShowCreateGroupModal(true)}
                className="btn-create-group-icon"
                title="Create New Group"
              >
                <i className="bi bi-plus-circle-fill"></i>
              </button>
            </div>
          </div>
          <div className="card-body">
            {data?.groups && data.groups.length > 0 ? (
              <div className="row g-2">
                {data.groups.map((group) => (
                  <div key={group.name} className="col-md-3 col-sm-6">
                    <Link
                      to={`/group/${encodeURIComponent(group.name)}`}
                      className="text-decoration-none"
                    >
                      <div className="card h-100 group-card">
                        <div className="card-body">
                          <h6 className="card-title">
                            <i className="bi bi-folder-fill"></i>
                            {group.name}
                          </h6>
                          <p className="card-text text-muted mb-0">
                            <i className="bi bi-box"></i>
                            {group.repositories} {group.repositories === 1 ? 'repo' : 'repos'}
                          </p>
                        </div>
                      </div>
                    </Link>
                  </div>
                ))}
              </div>
            ) : (
              <div className="empty-state">
                <i className="bi bi-folder-x"></i>
                <p className="mb-0">No groups found. Create a group to get started.</p>
              </div>
            )}
          </div>
        </div>
      </div>

      <CreateGroupModal
        show={showCreateGroupModal}
        onClose={() => setShowCreateGroupModal(false)}
        onCreated={handleGroupCreated}
      />
      
      <Footer />
    </div>
  );
}

export default Dashboard;

